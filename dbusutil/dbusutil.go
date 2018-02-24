package dbusutil

import (
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"

	"pkg.deepin.io/lib/dbus1"
	"pkg.deepin.io/lib/dbus1/introspect"
)

const orgFreedesktopDBus = "org.freedesktop.DBus"

type ExportInfo struct {
	Path, Interface string
}

func (ei ExportInfo) String() string {
	return fmt.Sprintf("<ExportInfo Path=%q Interface=%q>", ei.Path, ei.Interface)
}

type Exportable interface {
	GetDBusExportInfo() ExportInfo
}

type accessType uint

const (
	accessRead      accessType = 1
	accessWrite                = 2
	accessReadWrite            = accessRead | accessWrite
)

func (a accessType) String() string {
	switch a {
	case accessRead:
		return "read"
	case accessWrite:
		return "write"
	case accessReadWrite:
		return "readwrite"
	default:
		return fmt.Sprintf("invalid(%d)", a)
	}
}

type emitType uint

const (
	emitFalse emitType = iota
	emitTrue
	emitInvalidates
)

func (e emitType) String() string {
	switch e {
	case emitFalse:
		return "false"
	case emitTrue:
		return "true"
	case emitInvalidates:
		return "invalidates"
	default:
		return fmt.Sprintf("invalid(%d)", e)
	}
}

// struct field prop
type fieldProp struct {
	name        string
	rValue      reflect.Value
	rType       reflect.Type
	signature   dbus.Signature
	hasStruct   bool
	emit        emitType
	access      accessType
	valueLocker propLocker

	cbMu       sync.Mutex
	writeCb    PropertyWriteCallback
	readCb     PropertyReadCallback
	changedCbs []PropertyChangedCallback
}

func (p *fieldProp) getValue(propRead *PropertyRead) (value interface{}, err *dbus.Error) {
	readCb := p.ReadCallback()
	if readCb != nil {
		err = readCb(propRead)
		if err != nil {
			return
		}
	}

	if p.valueLocker != nil {
		p.valueLocker.RLock()
	}

	value = p.rValue.Interface()
	if propValue, ok := value.(Property); ok {
		value, err = propValue.GetValue()
	}

	if p.valueLocker != nil {
		p.valueLocker.RUnlock()
	}
	return
}

func (p *fieldProp) GetValueVariant(propRead *PropertyRead) (dbus.Variant, *dbus.Error) {
	value, err := p.getValue(propRead)
	if err != nil {
		return dbus.Variant{}, err
	}
	return dbus.MakeVariantWithSignature(value, p.signature), nil
}

func (p *fieldProp) SetValue(propWrite *PropertyWrite) (changed bool, err *dbus.Error) {
	writeCb := p.WriteCallback()
	if writeCb != nil {
		err = writeCb(propWrite)
		if err != nil {
			return
		}
	}

	if p.valueLocker != nil {
		p.valueLocker.Lock()
	}

	newVal := propWrite.Value

	value := p.rValue.Interface()
	propValue, ok := value.(Property)
	if ok {
		changed, err = propValue.SetValue(newVal)
	} else {
		newValRV := reflect.ValueOf(newVal)
		newValRT := reflect.TypeOf(newVal)
		valueRT := reflect.TypeOf(value)
		if valueRT != newValRT {
			// type not equal, try convert
			if newValRT.ConvertibleTo(valueRT) {
				newValRV = newValRV.Convert(valueRT)
			} else {
				err = dbus.MakeFailedError(errors.New("type not convertible"))
			}
		}

		if err == nil && !reflect.DeepEqual(value, newValRV.Interface()) {
			p.rValue.Set(newValRV)
			changed = true
		}
	}

	if p.valueLocker != nil {
		p.valueLocker.Unlock()
	}
	return
}

func (p *fieldProp) WriteCallback() PropertyWriteCallback {
	p.cbMu.Lock()
	cb := p.writeCb
	p.cbMu.Unlock()
	return cb
}

func (p *fieldProp) ReadCallback() PropertyReadCallback {
	p.cbMu.Lock()
	cb := p.readCb
	p.cbMu.Unlock()
	return cb
}

func (p *fieldProp) setWriteCallback(cb PropertyWriteCallback) {
	p.cbMu.Lock()
	p.writeCb = cb
	p.cbMu.Unlock()
}

func (p *fieldProp) setReadCallback(cb PropertyReadCallback) {
	p.cbMu.Lock()
	p.readCb = cb
	p.cbMu.Unlock()
}

func (p *fieldProp) connectChanged(cb PropertyChangedCallback) {
	p.cbMu.Lock()

	// copy on write
	newCbs := make([]PropertyChangedCallback, len(p.changedCbs)+1)
	copy(newCbs, p.changedCbs)
	newCbs[len(newCbs)-1] = cb
	p.changedCbs = newCbs

	p.cbMu.Unlock()
}

// do changed callbacks
func (p *fieldProp) notifyChanged(change *PropertyChanged) {
	p.cbMu.Lock()
	callbacks := p.changedCbs
	p.cbMu.Unlock()
	for _, cb := range callbacks {
		cb(change)
	}
}

// emit DBus signal Properties.PropertiesChanged
func (p *fieldProp) emitChanged(conn *dbus.Conn, path dbus.ObjectPath, interfaceName string,
	value interface{}) (err error) {
	const signal = orgFreedesktopDBus + ".Properties.PropertiesChanged"
	var changedProps map[string]dbus.Variant
	switch p.emit {
	case emitFalse:
		// do nothing
	case emitInvalidates:
		err = conn.Emit(path, signal, interfaceName, changedProps, []string{p.name})
	case emitTrue:
		changedProps = map[string]dbus.Variant{
			p.name: dbus.MakeVariant(value),
		}
		err = conn.Emit(path, signal, interfaceName, changedProps, []string{})
	default:
		panic("invalid value for emitType")
	}
	return
}

func getPropsIntrospection(props map[string]*fieldProp) []introspect.Property {
	var result = make([]introspect.Property, len(props))
	idx := 0
	for _, p := range props {

		var access string
		switch p.access {
		case accessWrite:
			access = "write"
		case accessRead:
			access = "read"
		case accessReadWrite:
			access = "readwrite"
		default:
			panic("invalid access")
		}

		result[idx] = introspect.Property{
			Name:   p.name,
			Type:   p.signature.String(),
			Access: access,
		}
		idx++
	}

	return result
}

func getSignals(structType reflect.Type) []introspect.Signal {
	signalsField, ok := structType.FieldByName("signals")
	if !ok {
		return nil
	}

	if signalsField.Type.Kind() != reflect.Ptr {
		return nil
	}

	signalsFieldElemType := signalsField.Type.Elem()
	if signalsFieldElemType.Kind() != reflect.Struct {
		return nil
	}

	var signals []introspect.Signal
	numField := signalsFieldElemType.NumField()
	for i := 0; i < numField; i++ {
		signalItem := signalsFieldElemType.Field(i)
		signalItemType := signalItem.Type

		if signalItemType.Kind() == reflect.Struct {
			var args []introspect.Arg
			numArg := signalItemType.NumField()
			for j := 0; j < numArg; j++ {
				signalArg := signalItemType.Field(j)
				args = append(args, introspect.Arg{
					Name: signalArg.Name,
					Type: dbus.SignatureOfType(signalArg.Type).String(),
				})
			}
			signals = append(signals, introspect.Signal{
				Name: signalItem.Name,
				Args: args,
			})
		}
	}
	return signals
}

const propsMasterField = "PropsMaster"

func getCorePropsLocker(structValue reflect.Value) propLocker {
	propsMasterRV := structValue.FieldByName(propsMasterField)
	if !propsMasterRV.IsValid() {
		return nil
	}
	return propsMasterRV.Addr().Interface().(*PropsMaster)
}

func getStructTypeValue(m interface{}) (reflect.Type, reflect.Value) {
	type0 := reflect.TypeOf(m)
	value0 := reflect.ValueOf(m)

	if type0.Kind() != reflect.Ptr {
		return nil, reflect.Value{}
	}

	elemType := type0.Elem()
	elemValue := value0.Elem()

	if elemType.Kind() != reflect.Struct {
		return nil, reflect.Value{}
	}

	return elemType, elemValue
}

func getProps(impl *implementer, structType reflect.Type,
	structValue reflect.Value) map[string]*fieldProp {

	props := make(map[string]*fieldProp)

	var prevField reflect.StructField
	numField := structType.NumField()
	for i := 0; i < numField; i++ {
		field := structType.Field(i)
		fieldValue := structValue.Field(i)

		if field.Name == propsMasterField {
			prevField = field
			continue
		}

		if !fieldValue.CanSet() {
			prevField = field
			continue
		}

		tag := field.Tag.Get("prop")
		if tag == "-" {
			prevField = field
			continue
		}

		// prev field: Prop1
		// this field: Prop1Mu
		if prevField.Name+"Mu" == field.Name {
			mu, ok := fieldValue.Addr().Interface().(*sync.RWMutex)
			if ok {
				// override prev fieldProp.mu
				props[prevField.Name].valueLocker = mu
			}

			prevField = field
			continue
		}

		prop0 := newFieldProp(field, fieldValue, tag, impl)
		props[field.Name] = prop0
		prevField = field
	}
	return props
}

func parsePropTag(tag string) (accessType, emitType) {
	access := accessRead
	emit := emitTrue
	tagParts := strings.Split(tag, ",")
	for _, tagPart := range tagParts {
		if strings.HasPrefix(tagPart, "access:") {
			accessStr := tagPart[len("access:"):]
			switch accessStr {
			case "r", "read":
				access = accessRead
			case "w", "write":
				access = accessWrite
			case "rw", "readwrite":
				access = accessReadWrite
			default:
				panic(fmt.Errorf("invalid access %q", accessStr))
			}
			continue
		} else if strings.HasPrefix(tagPart, "emit:") {
			emitStr := tagPart[len("emit:"):]
			switch emitStr {
			case "true":
				emit = emitTrue
			case "false":
				emit = emitFalse
			case "invalidates":
				emit = emitInvalidates
			default:
				panic(fmt.Errorf("invalid emit %q", emitStr))
			}
			continue
		}
	}
	return access, emit
}

func newFieldProp(field reflect.StructField, fieldValue reflect.Value, tag string,
	impl *implementer) *fieldProp {

	access, emit := parsePropTag(tag)
	p := &fieldProp{
		name:   field.Name,
		rValue: fieldValue,
		access: access,
		emit:   emit,
	}
	var rType reflect.Type
	propValue, ok := fieldValue.Interface().(Property)
	if ok {
		log.Printf("propValue %T %#v\n", propValue, propValue)
		propValue.SetNotifyChangedFunc(func(val interface{}) {
			impl.notifyChanged(p, val)
		})

		rType = propValue.GetType()
		p.valueLocker = nil
	} else {
		rType = field.Type
		p.valueLocker = impl.corePropsLocker
	}
	p.rType = rType
	p.signature = dbus.SignatureOfType(rType)
	if strings.Contains(p.signature.String(), "(") {
		p.hasStruct = true
	}
	return p
}

type methodDetail struct {
	In  []string
	Out []string
}

func (md methodDetail) getInArgName(index int, type0 reflect.Type, methodName string) string {
	if index >= len(md.In) {
		panic(fmt.Errorf("failed to get %s.%s in[%d] argument name",
			type0, methodName, index))
	}
	return md.In[index]
}

func (md methodDetail) getOutArgName(index int, type0 reflect.Type, methodName string) string {
	if index >= len(md.Out) {
		panic(fmt.Errorf("failed to get %s.%s out[%d] argument name",
			type0, methodName, index))
	}
	return md.Out[index]
}

func getMethodDetailMap(structType reflect.Type) map[string]methodDetail {
	result := make(map[string]methodDetail)
	methodsField, ok := structType.FieldByName("methods")
	if !ok {
		return nil
	}

	if methodsField.Type.Kind() != reflect.Ptr {
		return nil
	}

	methodsFieldElemType := methodsField.Type.Elem()
	if methodsFieldElemType.Kind() != reflect.Struct {
		return nil
	}

	numField := methodsFieldElemType.NumField()
	for i := 0; i < numField; i++ {
		methodItem := methodsFieldElemType.Field(i)
		tagIn := methodItem.Tag.Get("in")
		tagOut := methodItem.Tag.Get("out")

		result[methodItem.Name] = methodDetail{
			In:  splitArg(tagIn),
			Out: splitArg(tagOut),
		}
	}
	return result
}

func splitArg(str string) (result []string) {
	parts := strings.Split(str, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return
}

// Methods returns the description of the methods of v. This can be used to
// create a Node which can be passed to NewIntrospectable.
func getMethods(v interface{}, methodDetailMap map[string]methodDetail) []introspect.Method {
	t := reflect.TypeOf(v)
	ms := make([]introspect.Method, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		if t.Method(i).PkgPath != "" {
			continue
		}
		mt := t.Method(i).Type
		if mt.NumOut() == 0 ||
			mt.Out(mt.NumOut()-1) != reflect.TypeOf(&dbus.Error{}) {

			continue
		}
		var m introspect.Method
		m.Name = t.Method(i).Name
		m.Args = make([]introspect.Arg, 0, mt.NumIn()+mt.NumOut()-2)

		methodDetail := methodDetailMap[m.Name]
		inArgIndex := 0
		for j := 1; j < mt.NumIn(); j++ {
			if mt.In(j) != reflect.TypeOf((*dbus.Sender)(nil)).Elem() &&
				mt.In(j) != reflect.TypeOf((*dbus.Message)(nil)).Elem() {

				argName := methodDetail.getInArgName(inArgIndex, t, m.Name)
				inArgIndex++
				arg := introspect.Arg{Name: argName,
					Type:      dbus.SignatureOfType(mt.In(j)).String(),
					Direction: "in",
				}
				m.Args = append(m.Args, arg)
			}
		}
		for j := 0; j < mt.NumOut()-1; j++ {
			argName := methodDetail.getOutArgName(j, t, m.Name)
			arg := introspect.Arg{
				Name:      argName,
				Type:      dbus.SignatureOfType(mt.Out(j)).String(),
				Direction: "out",
			}
			m.Args = append(m.Args, arg)
		}
		m.Annotations = make([]introspect.Annotation, 0)
		ms = append(ms, m)
	}
	return ms
}

type PropertyReadCallback func(read *PropertyRead) *dbus.Error

type PropertyWriteCallback func(write *PropertyWrite) *dbus.Error

type PropertyChangedCallback func(change *PropertyChanged)

type Property interface {
	SetValue(val interface{}) (changed bool, err *dbus.Error)
	GetValue() (val interface{}, err *dbus.Error)
	SetNotifyChangedFunc(func(val interface{}))
	GetType() reflect.Type
}

type PropertyInfo struct {
	Path      dbus.ObjectPath
	Interface string
	Name      string
}

type PropertyAccess struct {
	PropertyInfo
	Sender  dbus.Sender
	service *Service
}

func (pa *PropertyAccess) GetPID() (uint32, error) {
	return pa.service.GetConnPID(string(pa.Sender))
}

func (pa *PropertyAccess) GetUID() (uint32, error) {
	return pa.service.GetConnUID(string(pa.Sender))
}

type PropertyRead struct {
	PropertyAccess
}

func newPropertyRead(name string, impl *implementer, sender dbus.Sender) *PropertyRead {
	pr := new(PropertyRead)
	pr.Sender = sender
	pr.service = impl.service
	pr.Name = name
	pr.Interface = impl.interfaceName
	pr.Path = impl.path
	return pr
}

type PropertyWrite struct {
	PropertyAccess
	Value interface{} // new value
}

func newPropertyWrite(name string, impl *implementer, value interface{},
	sender dbus.Sender) *PropertyWrite {

	pw := new(PropertyWrite)
	pw.Sender = sender
	pw.service = impl.service
	pw.Name = name
	pw.Interface = impl.interfaceName
	pw.Path = impl.path
	pw.Value = value
	return pw
}

type PropertyChanged struct {
	PropertyInfo
	Value interface{} // new value
}

func newPropertyChanged(name string, impl *implementer, value interface{}) *PropertyChanged {
	pc := new(PropertyChanged)
	pc.Name = name
	pc.Interface = impl.interfaceName
	pc.Path = impl.path
	pc.Value = value
	return pc
}

func valueFromBus(src interface{}, valueRT reflect.Type) (reflect.Value, error) {
	newValueRV := reflect.New(valueRT)
	err := dbus.Store([]interface{}{src}, newValueRV.Interface())
	if err != nil {
		return reflect.Value{}, err
	}
	return newValueRV.Elem(), nil
}

type propLocker interface {
	sync.Locker
	RLock()
	RUnlock()
}

type PropsMaster struct {
	sync.RWMutex

	mu          sync.Mutex
	delayModeMu sync.Mutex
	idx         int
	changes     []propsMasterChanged
}

type propsMasterChanged struct {
	name  string
	value interface{}
}

func (pm *PropsMaster) index() int {
	return pm.idx - 1
}

func (pm *PropsMaster) NotifyChanged(v Exportable, service *Service, propName string, value interface{}) error {
	if service == nil {
		return nil
	}
	exportInfo := v.GetDBusExportInfo()
	impl := service.getImplementer(exportInfo)

	if impl == nil {
		return nil
	}

	pm.mu.Lock()
	if pm.index() < 0 {
		pm.mu.Unlock()

		prop0 := impl.getProperty(propName)
		if prop0 == nil {
			return errors.New("not found property")
		}
		return prop0.emitChanged(service.conn, impl.path, impl.interfaceName, value)
	}
	pm.addChanged(propName, value)
	pm.mu.Unlock()
	return nil
}

func (pm *PropsMaster) addChanged(propName string, value interface{}) {
	index := pm.index()
	if index > len(pm.changes) {
		panic("pm.index out of range")
	}

	c := propsMasterChanged{propName, value}
	for i := 0; i < index; i++ {
		change := pm.changes[i]
		if change.name == propName {
			pm.changes[i] = c
			return
		}
	}
	if index+1 <= len(pm.changes) {
		pm.changes[index] = c
	} else {
		pm.changes = append(pm.changes, c)
	}
	pm.idx++
}

// Begin enter delay mode
func (pm *PropsMaster) Begin() {
	pm.delayModeMu.Lock()
	pm.mu.Lock()
	if pm.index() == -1 {
		pm.idx = 1
	} else {
		panic("pm.index != -1")
	}
	pm.mu.Unlock()
}

// End quit delay mode
func (pm *PropsMaster) End(v Exportable, service *Service) (err error) {
	pm.mu.Lock()
	defer func() {
		pm.idx = 0
		pm.mu.Unlock()
		pm.delayModeMu.Unlock()
	}()

	if service == nil {
		return nil
	}
	exportInfo := v.GetDBusExportInfo()
	path := dbus.ObjectPath(exportInfo.Path)
	impl := service.getImplementer(exportInfo)
	if impl == nil {
		// v is not exported
		return nil
	}

	index := pm.index()
	if index < 0 || index > len(pm.changes) {
		panic("pm.index out of range")
	}

	var changedProps map[string]dbus.Variant
	var invalidatedProps []string
	if index > 0 {
		changedProps = make(map[string]dbus.Variant)
	}
	for i := 0; i < index; i++ {
		change := pm.changes[i]
		p := impl.props[change.name]
		switch p.emit {
		case emitFalse:
			// do nothing
		case emitInvalidates:
			invalidatedProps = append(invalidatedProps, change.name)
		case emitTrue:
			changedProps[change.name] = dbus.MakeVariant(change.value)
		}
	}

	const signalName = orgFreedesktopDBus + ".Properties.PropertiesChanged"
	if len(changedProps)+len(invalidatedProps) > 0 {
		err = service.conn.Emit(path, signalName, exportInfo.Interface,
			changedProps, invalidatedProps)
	}
	return
}