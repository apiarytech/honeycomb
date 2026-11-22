/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package opcua

import (
	"fmt"
	"strings"
	"uuid"

	ua "github.com/apache/plc4x/plc4go/protocols/opcua/readwrite/model"
)

// Node ids of namespace 0 (OPC UA Part 6, NodeIds.csv) the server uses.
const (
	idBoolean                = 1
	idByteString             = 15
	idNodeIdType             = 17
	idStatusCodeType         = 19
	idLocalizedText          = 21
	idStructure              = 22
	idBaseDataType           = 24
	idNumber                 = 26
	idInteger                = 27
	idUInteger               = 28
	idEnumeration            = 29
	idReferences             = 31
	idNonHierarchical        = 32
	idHierarchical           = 33
	idHasChild               = 34
	idOrganizes              = 35
	idHasEventSource         = 36
	idHasEncoding            = 38
	idHasTypeDefinition      = 40
	idAggregates             = 44
	idHasSubtype             = 45
	idHasProperty            = 46
	idHasComponent           = 47
	idHasNotifier            = 48
	idBaseObjectType         = 58
	idFolderType             = 61
	idBaseVariableType       = 62
	idBaseDataVariableType   = 63
	idPropertyType           = 68
	idDataTypeEncodingType   = 76
	idRoot                   = 84
	idObjects                = 85
	idTypes                  = 86
	idViews                  = 87
	idObjectTypes            = 88
	idVariableTypes          = 89
	idDataTypes              = 90
	idReferenceTypes         = 91
	idArgument               = 296
	idArgumentBinary         = 298
	idStructureDefinitionBin = 122
	idServerStateType        = 852
	idServerStatusDataType   = 862
	idServerType             = 2004
	idBaseEventType          = 2041
	idSystemEventType        = 2130
	idServerStatusType       = 2138
	idServer                 = 2253
	idServerArray            = 2254
	idNamespaceArray         = 2255
	idServerStatus           = 2256
	idServerStatusStartTime  = 2257
	idServerCurrentTime      = 2258
	idServerState            = 2259
	idStateVariableType      = 2755
	idConditionType          = 2782
	idRefreshStartEventType  = 2787
	idRefreshEndEventType    = 2788
	idAcknowledgeableCond    = 2881
	idAlarmConditionType     = 2915
	idConditionRefresh       = 3875
	idHasCondition           = 9006
	idTwoStateVariableType   = 8995
	idEnable                 = 9027
	idDisable                = 9028
	idAddComment             = 9029
	idAcknowledge            = 9111
	idConfirm                = 9113
	idBaseConditionClassType = 11163
)

// superTypes are the reference types' supertypes, for a Browse with subtypes.
var superTypes = map[uint32]uint32{
	idNonHierarchical: idReferences, idHierarchical: idReferences, idHasChild: idHierarchical,
	idOrganizes: idHierarchical, idAggregates: idHasChild, idHasSubtype: idHasChild,
	idHasProperty: idAggregates, idHasComponent: idAggregates, idHasTypeDefinition: idNonHierarchical,
	idHasEventSource: idHierarchical, idHasNotifier: idHasEventSource, idHasEncoding: idNonHierarchical,
	idHasCondition: idNonHierarchical,
}

func isSubtype(referenceType, of uint32) bool {
	for t := referenceType; ; {
		if t == of {
			return true
		}
		parent, ok := superTypes[t]
		if !ok {
			return false
		}
		t = parent
	}
}

// eventSuperTypes are the event types' supertypes.
var eventSuperTypes = map[uint32]uint32{
	idConditionType: idBaseEventType, idAcknowledgeableCond: idConditionType, idAlarmConditionType: idAcknowledgeableCond,
	idSystemEventType: idBaseEventType, idRefreshStartEventType: idSystemEventType, idRefreshEndEventType: idSystemEventType,
}

func isEventSubtype(eventType, of uint32) bool {
	for t := eventType; ; {
		if t == of {
			return true
		}
		parent, ok := eventSuperTypes[t]
		if !ok {
			return false
		}
		t = parent
	}
}

// Attribute ids (OPC UA Part 6, A.1).
const (
	attrNodeId                  = 1
	attrNodeClass               = 2
	attrBrowseName              = 3
	attrDisplayName             = 4
	attrDescription             = 5
	attrWriteMask               = 6
	attrUserWriteMask           = 7
	attrIsAbstract              = 8
	attrSymmetric               = 9
	attrInverseName             = 10
	attrEventNotifier           = 12
	attrValue                   = 13
	attrDataType                = 14
	attrValueRank               = 15
	attrArrayDimensions         = 16
	attrAccessLevel             = 17
	attrUserAccessLevel         = 18
	attrMinimumSamplingInterval = 19
	attrHistorizing             = 20
	attrExecutable              = 21
	attrUserExecutable          = 22
	attrDataTypeDefinition      = 23
)

// EventNotifier bits.
const (
	notifierSubscribe   = 1
	notifierHistoryRead = 4
)

// reference is one reference of a node.
type reference struct {
	typeId  uint32 // in namespace 0
	target  string // the target's key
	forward bool
}

// node is a node of the address space.
type node struct {
	key         string
	id          ua.NodeId
	class       ua.NodeClass
	namespace   uint16 // of the browse name
	name        string
	description string
	typeDef     uint32
	references  []reference

	// Variables: a point, a member of a structure point (path), or one of the server's own
	// (variant gives its value).
	dataType   uint32
	dataTypeId ua.NodeId // the DataType attribute, when it is not a namespace-0 number
	structure  *structureType
	valueRank  int32
	dims       []uint32
	point      *Point
	path       []string
	variant    func() ua.Variant

	// Types.
	isAbstract  bool
	symmetric   bool
	inverseName string
	definition  *structureType // a structured DataType's

	// Methods and objects.
	method        *method
	eventNotifier uint8
	alarm         *alarm
}

// keyOf is the key of a node id: "ns=0;i=85", "ns=1;s=Line1", "ns=1;g=…", "ns=1;b=…".
func keyOf(id ua.NodeId) string {
	if id == nil {
		return ""
	}
	return keyOfDefinition(id.GetNodeId())
}

func keyOfDefinition(definition ua.NodeIdTypeDefinition) string {
	switch d := definition.(type) {
	case ua.NodeIdTwoByte:
		return fmt.Sprintf("ns=0;i=%d", d.GetId())
	case ua.NodeIdFourByte:
		return fmt.Sprintf("ns=%d;i=%d", d.GetNamespaceIndex(), d.GetId())
	case ua.NodeIdNumeric:
		return fmt.Sprintf("ns=%d;i=%d", d.GetNamespaceIndex(), d.GetId())
	case ua.NodeIdString:
		value := ""
		if d.GetId() != nil && d.GetId().GetStringValue() != nil {
			value = *d.GetId().GetStringValue()
		}
		return fmt.Sprintf("ns=%d;s=%s", d.GetNamespaceIndex(), value)
	case ua.NodeIdGuid:
		if len(d.GetId()) == 16 {
			return fmt.Sprintf("ns=%d;g=%s", d.GetNamespaceIndex(), uuid.UUID(d.GetId()))
		}
	case ua.NodeIdByteString:
		if d.GetId() != nil {
			return fmt.Sprintf("ns=%d;b=%x", d.GetNamespaceIndex(), d.GetId().GetStringValue())
		}
	}
	return ""
}

func numericKey(id uint32) string {
	return fmt.Sprintf("ns=0;i=%d", id)
}

func numericNodeId(namespace uint16, id uint32) ua.NodeId {
	switch {
	case namespace == 0 && id < 256:
		return ua.NewNodeId(ua.NewNodeIdTwoByte(uint8(id)))
	case namespace < 256 && id < 65536:
		return ua.NewNodeId(ua.NewNodeIdFourByte(uint8(namespace), uint16(id)))
	}
	return ua.NewNodeId(ua.NewNodeIdNumeric(namespace, id))
}

func stringNodeId(namespace uint16, id string) ua.NodeId {
	return ua.NewNodeId(ua.NewNodeIdString(namespace, ua.NewPascalString(&id)))
}

func nullNodeId() ua.NodeId {
	return ua.NewNodeId(ua.NewNodeIdTwoByte(0))
}

// Keys of the nodes of a structured type, out of the way of the points' names.
func typeKey(name string) string     { return "ns=1;s=type:" + name }
func encodingKey(name string) string { return "ns=1;s=encoding:" + name }

// addressSpace is the server's nodes: the standard ones a client expects (Root, Objects, the
// Server object, the type tree) and, under Objects, a folder per dotted prefix of the names of
// points, methods and alarms.
type addressSpace struct {
	nodes map[string]*node
}

func (a *addressSpace) add(n *node) *node {
	a.nodes[n.key] = n
	return n
}

// link adds a reference and its inverse.
func (a *addressSpace) link(source string, referenceType uint32, target string) {
	a.nodes[source].references = append(a.nodes[source].references, reference{typeId: referenceType, target: target, forward: true})
	a.nodes[target].references = append(a.nodes[target].references, reference{typeId: referenceType, target: source, forward: false})
}

func (a *addressSpace) standard(id uint32, class ua.NodeClass, name string, typeDef uint32) *node {
	return a.add(&node{key: numericKey(id), id: numericNodeId(0, id), class: class, name: name, typeDef: typeDef})
}

// typeNode adds a type of namespace 0 as a subtype of super (0 for none).
func (a *addressSpace) typeNode(id uint32, class ua.NodeClass, name string, super uint32, abstract bool) *node {
	n := a.standard(id, class, name, 0)
	n.isAbstract = abstract
	if super != 0 {
		a.link(numericKey(super), idHasSubtype, n.key)
	}
	return n
}

// declaration adds a variable of a type (an instance declaration): a property, or a
// component of type typeDef.
func (a *addressSpace) declaration(id, parent uint32, name string, dataType uint32, valueRank int32, typeDef uint32) *node {
	n := a.standard(id, ua.NodeClass_nodeClassVariable, name, typeDef)
	n.dataType, n.valueRank = dataType, valueRank
	referenceType := uint32(idHasComponent)
	if typeDef == idPropertyType {
		referenceType = idHasProperty
	}
	a.link(numericKey(parent), referenceType, n.key)
	return n
}

// buildTypes adds the Types folder: the data, object, variable and reference types the
// server's nodes and events use.
func (a *addressSpace) buildTypes() {
	folder := func(id uint32, name string, parent uint32) {
		a.standard(id, ua.NodeClass_nodeClassObject, name, idFolderType)
		a.link(numericKey(parent), idOrganizes, numericKey(id))
	}
	folder(idObjectTypes, "ObjectTypes", idTypes)
	folder(idVariableTypes, "VariableTypes", idTypes)
	folder(idDataTypes, "DataTypes", idTypes)
	folder(idReferenceTypes, "ReferenceTypes", idTypes)

	reference := func(id uint32, name string, super uint32, abstract, symmetric bool, inverse string) {
		n := a.typeNode(id, ua.NodeClass_nodeClassReferenceType, name, super, abstract)
		n.symmetric, n.inverseName = symmetric, inverse
	}
	reference(idReferences, "References", 0, true, true, "")
	a.link(numericKey(idReferenceTypes), idOrganizes, numericKey(idReferences))
	reference(idHierarchical, "HierarchicalReferences", idReferences, true, false, "InverseHierarchicalReferences")
	reference(idNonHierarchical, "NonHierarchicalReferences", idReferences, true, true, "")
	reference(idHasChild, "HasChild", idHierarchical, true, false, "ChildOf")
	reference(idOrganizes, "Organizes", idHierarchical, false, false, "OrganizedBy")
	reference(idHasEventSource, "HasEventSource", idHierarchical, false, false, "EventSourceOf")
	reference(idHasNotifier, "HasNotifier", idHasEventSource, false, false, "NotifierOf")
	reference(idAggregates, "Aggregates", idHasChild, true, false, "AggregatedBy")
	reference(idHasSubtype, "HasSubtype", idHasChild, false, false, "SubtypeOf")
	reference(idHasComponent, "HasComponent", idAggregates, false, false, "ComponentOf")
	reference(idHasProperty, "HasProperty", idAggregates, false, false, "PropertyOf")
	reference(idHasTypeDefinition, "HasTypeDefinition", idNonHierarchical, false, false, "TypeDefinitionOf")
	reference(idHasEncoding, "HasEncoding", idNonHierarchical, false, false, "EncodingOf")
	reference(idHasCondition, "HasCondition", idNonHierarchical, false, false, "IsConditionOf")

	dataType := func(id uint32, name string, super uint32, abstract bool) {
		a.typeNode(id, ua.NodeClass_nodeClassDataType, name, super, abstract)
	}
	dataType(idBaseDataType, "BaseDataType", 0, true)
	a.link(numericKey(idDataTypes), idOrganizes, numericKey(idBaseDataType))
	dataType(idBoolean, "Boolean", idBaseDataType, false)
	dataType(idNumber, "Number", idBaseDataType, true)
	dataType(idInteger, "Integer", idNumber, true)
	dataType(idUInteger, "UInteger", idNumber, true)
	for _, t := range []struct {
		id    uint32
		name  string
		super uint32
	}{
		{TypeSByte, "SByte", idInteger}, {TypeInt16, "Int16", idInteger}, {TypeInt32, "Int32", idInteger}, {TypeInt64, "Int64", idInteger},
		{TypeByte, "Byte", idUInteger}, {TypeUInt16, "UInt16", idUInteger}, {TypeUInt32, "UInt32", idUInteger}, {TypeUInt64, "UInt64", idUInteger},
		{TypeFloat, "Float", idNumber}, {TypeDouble, "Double", idNumber}, {TypeString, "String", idBaseDataType},
		{TypeDateTime, "DateTime", idBaseDataType}, {idByteString, "ByteString", idBaseDataType}, {idNodeIdType, "NodeId", idBaseDataType},
		{idStatusCodeType, "StatusCode", idBaseDataType}, {idLocalizedText, "LocalizedText", idBaseDataType},
	} {
		dataType(t.id, t.name, t.super, false)
	}
	dataType(idStructure, "Structure", idBaseDataType, true)
	dataType(idEnumeration, "Enumeration", idBaseDataType, true)
	dataType(idServerStateType, "ServerState", idEnumeration, false)
	dataType(idServerStatusDataType, "ServerStatusDataType", idStructure, false)
	dataType(idArgument, "Argument", idStructure, false)

	objectType := func(id uint32, name string, super uint32, abstract bool) {
		a.typeNode(id, ua.NodeClass_nodeClassObjectType, name, super, abstract)
	}
	objectType(idBaseObjectType, "BaseObjectType", 0, false)
	a.link(numericKey(idObjectTypes), idOrganizes, numericKey(idBaseObjectType))
	objectType(idFolderType, "FolderType", idBaseObjectType, false)
	objectType(idServerType, "ServerType", idBaseObjectType, false)
	objectType(idDataTypeEncodingType, "DataTypeEncodingType", idBaseObjectType, false)
	objectType(idBaseEventType, "BaseEventType", idBaseObjectType, true)
	objectType(idSystemEventType, "SystemEventType", idBaseEventType, true)
	objectType(idRefreshStartEventType, "RefreshStartEventType", idSystemEventType, false)
	objectType(idRefreshEndEventType, "RefreshEndEventType", idSystemEventType, false)
	objectType(idConditionType, "ConditionType", idBaseEventType, true)
	objectType(idAcknowledgeableCond, "AcknowledgeableConditionType", idConditionType, false)
	objectType(idAlarmConditionType, "AlarmConditionType", idAcknowledgeableCond, false)
	objectType(idBaseConditionClassType, "BaseConditionClassType", idBaseObjectType, true)

	variableType := func(id uint32, name string, super uint32, abstract bool) {
		n := a.typeNode(id, ua.NodeClass_nodeClassVariableType, name, super, abstract)
		n.dataType, n.valueRank = idBaseDataType, -2
	}
	variableType(idBaseVariableType, "BaseVariableType", 0, true)
	a.link(numericKey(idVariableTypes), idOrganizes, numericKey(idBaseVariableType))
	variableType(idBaseDataVariableType, "BaseDataVariableType", idBaseVariableType, false)
	variableType(idPropertyType, "PropertyType", idBaseVariableType, false)
	variableType(idServerStatusType, "ServerStatusType", idBaseDataVariableType, false)
	variableType(idStateVariableType, "StateVariableType", idBaseDataVariableType, true)
	variableType(idTwoStateVariableType, "TwoStateVariableType", idStateVariableType, false)

	// The fields of events, as instance declarations a client's event filter names.
	property := func(id, parent uint32, name string, dataType uint32) {
		a.declaration(id, parent, name, dataType, -1, idPropertyType)
	}
	property(2042, idBaseEventType, "EventId", idByteString)
	property(2043, idBaseEventType, "EventType", idNodeIdType)
	property(2044, idBaseEventType, "SourceNode", idNodeIdType)
	property(2045, idBaseEventType, "SourceName", TypeString)
	property(2046, idBaseEventType, "Time", TypeDateTime)
	property(2047, idBaseEventType, "ReceiveTime", TypeDateTime)
	property(2050, idBaseEventType, "Message", idLocalizedText)
	property(2051, idBaseEventType, "Severity", TypeUInt16)
	property(11112, idConditionType, "ConditionClassId", idNodeIdType)
	property(11113, idConditionType, "ConditionClassName", idLocalizedText)
	property(9009, idConditionType, "ConditionName", TypeString)
	property(9010, idConditionType, "BranchId", idNodeIdType)
	property(3874, idConditionType, "Retain", idBoolean)
	property(9026, idConditionType, "ClientUserId", TypeString)
	twoState := func(id, idOfId, parent uint32, name string) {
		a.declaration(id, parent, name, idLocalizedText, -1, idTwoStateVariableType)
		property(idOfId, id, "Id", idBoolean)
	}
	twoState(9011, 9012, idConditionType, "EnabledState")
	a.declaration(9024, idConditionType, "Comment", idLocalizedText, -1, idBaseDataVariableType)
	a.declaration(9022, idConditionType, "LastSeverity", TypeUInt16, -1, idBaseDataVariableType)
	twoState(9093, 9094, idAcknowledgeableCond, "AckedState")
	twoState(9102, 9103, idAcknowledgeableCond, "ConfirmedState")
	twoState(9160, 9161, idAlarmConditionType, "ActiveState")

	// The methods of conditions.
	conditionMethod := func(id, parent uint32, name string, inputsId uint32, inputs ...Argument) {
		n := a.standard(id, ua.NodeClass_nodeClassMethod, name, 0)
		n.method = &method{executable: true, cfg: Method{Name: name, Inputs: inputs}}
		a.link(numericKey(parent), idHasComponent, n.key)
		if len(inputs) > 0 {
			args := a.declaration(inputsId, id, "InputArguments", idArgument, 1, idPropertyType)
			args.dims = []uint32{uint32(len(inputs))}
			args.variant = func() ua.Variant { return argumentsVariant(inputs) }
		}
	}
	eventId := Argument{Name: "EventId", DataType: idByteString, Description: "the event to act on"}
	comment := Argument{Name: "Comment", DataType: idLocalizedText, Description: "a comment kept with the condition"}
	conditionMethod(idEnable, idConditionType, "Enable", 0)
	conditionMethod(idDisable, idConditionType, "Disable", 0)
	conditionMethod(idAddComment, idConditionType, "AddComment", 9030, eventId, comment)
	conditionMethod(idConditionRefresh, idConditionType, "ConditionRefresh", 3876,
		Argument{Name: "SubscriptionId", DataType: TypeUInt32, Description: "the subscription to refresh"})
	conditionMethod(idAcknowledge, idAcknowledgeableCond, "Acknowledge", 9112, eventId, comment)
	conditionMethod(idConfirm, idAcknowledgeableCond, "Confirm", 9114, eventId, comment)
}

// place finds or makes the folders of a dotted name under Objects and returns the key of the
// parent and the last part of the name.
func (a *addressSpace) place(name, what string) (parent, leaf string, err error) {
	if name == "" || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") {
		return "", "", fmt.Errorf("%s %q: a name is dot-separated parts, none empty", what, name)
	}
	parts := strings.Split(name, ".")
	parent = numericKey(idObjects)
	for depth := 1; depth < len(parts); depth++ {
		folderName := strings.Join(parts[:depth], ".")
		key := "ns=1;s=" + folderName
		folder, exists := a.nodes[key]
		if !exists {
			a.add(&node{key: key, id: stringNodeId(1, folderName), class: ua.NodeClass_nodeClassObject,
				namespace: 1, name: parts[depth-1], typeDef: idFolderType})
			a.link(parent, idOrganizes, key)
		} else if folder.class != ua.NodeClass_nodeClassObject || folder.alarm != nil {
			return "", "", fmt.Errorf("%s %q: %s is not a folder", what, name, folderName)
		}
		parent = key
	}
	if _, exists := a.nodes["ns=1;s="+name]; exists {
		return "", "", fmt.Errorf("%s %q: the name is taken", what, name)
	}
	return parent, parts[len(parts)-1], nil
}

// newAddressSpace builds the nodes. status gives the server's own variables their values.
func newAddressSpace(s *Server) (*addressSpace, error) {
	a := &addressSpace{nodes: map[string]*node{}}
	a.standard(idRoot, ua.NodeClass_nodeClassObject, "Root", idFolderType)
	a.standard(idObjects, ua.NodeClass_nodeClassObject, "Objects", idFolderType)
	a.standard(idTypes, ua.NodeClass_nodeClassObject, "Types", idFolderType)
	a.standard(idViews, ua.NodeClass_nodeClassObject, "Views", idFolderType)
	a.link(numericKey(idRoot), idOrganizes, numericKey(idObjects))
	a.link(numericKey(idRoot), idOrganizes, numericKey(idTypes))
	a.link(numericKey(idRoot), idOrganizes, numericKey(idViews))
	a.buildTypes()

	server := a.standard(idServer, ua.NodeClass_nodeClassObject, "Server", idServerType)
	server.eventNotifier = notifierSubscribe
	if s.cfg.History.Events > 0 {
		server.eventNotifier |= notifierHistoryRead
	}
	a.link(numericKey(idObjects), idOrganizes, numericKey(idServer))
	serverStatus := a.standard(idServerStatus, ua.NodeClass_nodeClassVariable, "ServerStatus", idServerStatusType)
	serverStatus.dataType, serverStatus.valueRank = idServerStatusDataType, -1
	a.link(numericKey(idServer), idHasComponent, numericKey(idServerStatus))
	serverVariable := func(id uint32, parent uint32, name string, dataType uint32, valueRank int32, typeDef, referenceType uint32) {
		n := a.standard(id, ua.NodeClass_nodeClassVariable, name, typeDef)
		n.dataType, n.valueRank = dataType, valueRank
		n.variant = func() ua.Variant { return s.serverValue(id) }
		a.link(numericKey(parent), referenceType, n.key)
	}
	serverVariable(idNamespaceArray, idServer, "NamespaceArray", TypeString, 1, idPropertyType, idHasProperty)
	serverVariable(idServerArray, idServer, "ServerArray", TypeString, 1, idPropertyType, idHasProperty)
	serverVariable(idServerStatusStartTime, idServerStatus, "StartTime", TypeDateTime, -1, idBaseDataVariableType, idHasComponent)
	serverVariable(idServerCurrentTime, idServerStatus, "CurrentTime", TypeDateTime, -1, idBaseDataVariableType, idHasComponent)
	serverVariable(idServerState, idServerStatus, "State", idServerStateType, -1, idBaseDataVariableType, idHasComponent)

	for _, t := range s.structures {
		dt := a.add(&node{key: typeKey(t.cfg.Name), id: t.id, class: ua.NodeClass_nodeClassDataType, namespace: 1,
			name: t.cfg.Name, description: t.cfg.Description, definition: t})
		a.link(numericKey(idStructure), idHasSubtype, dt.key)
		enc := a.add(&node{key: encodingKey(t.cfg.Name), id: t.encoding, class: ua.NodeClass_nodeClassObject,
			name: "Default Binary", typeDef: idDataTypeEncodingType})
		a.link(dt.key, idHasEncoding, enc.key)
	}

	for i := range s.cfg.Points {
		p := &s.cfg.Points[i]
		parent, leaf, err := a.place(p.Name, "point")
		if err != nil {
			return nil, err
		}
		n := a.add(&node{key: "ns=1;s=" + p.Name, id: stringNodeId(1, p.Name), class: ua.NodeClass_nodeClassVariable, namespace: 1,
			name: leaf, description: p.Description, typeDef: idBaseDataVariableType, dataType: p.DataType, valueRank: -1, point: p})
		if p.ArrayLength > 0 {
			n.valueRank, n.dims = 1, []uint32{uint32(p.ArrayLength)}
		}
		a.link(parent, idOrganizes, n.key)
		if p.DataType == TypeStructure {
			t := s.structureByName[p.Structure]
			n.structure, n.dataTypeId = t, t.id
			if err := a.members(n, t, nil); err != nil {
				return nil, err
			}
		}
	}

	for i := range s.methods {
		m := s.methods[i]
		parent, leaf, err := a.place(m.cfg.Name, "method")
		if err != nil {
			return nil, err
		}
		n := a.add(&node{key: "ns=1;s=" + m.cfg.Name, id: stringNodeId(1, m.cfg.Name), class: ua.NodeClass_nodeClassMethod,
			namespace: 1, name: leaf, description: m.cfg.Description, method: m})
		m.parent = parent
		a.link(parent, idHasComponent, n.key)
		for _, args := range []struct {
			name string
			list []Argument
		}{{"InputArguments", m.cfg.Inputs}, {"OutputArguments", m.cfg.Outputs}} {
			if len(args.list) == 0 {
				continue
			}
			list := args.list
			key := n.key + "." + args.name
			if _, taken := a.nodes[key]; taken {
				return nil, fmt.Errorf("method %q: %s is taken", m.cfg.Name, key)
			}
			prop := a.add(&node{key: key, id: stringNodeId(1, m.cfg.Name+"."+args.name), class: ua.NodeClass_nodeClassVariable,
				name: args.name, typeDef: idPropertyType, dataType: idArgument, valueRank: 1, dims: []uint32{uint32(len(list))},
				variant: func() ua.Variant { return argumentsVariant(list) }})
			a.link(n.key, idHasProperty, prop.key)
		}
	}

	for _, al := range s.alarms {
		parent, leaf, err := a.place(al.cfg.Name, "alarm")
		if err != nil {
			return nil, err
		}
		n := a.add(&node{key: "ns=1;s=" + al.cfg.Name, id: stringNodeId(1, al.cfg.Name), class: ua.NodeClass_nodeClassObject,
			namespace: 1, name: leaf, description: al.cfg.Message, typeDef: idAlarmConditionType, alarm: al})
		al.node = n
		a.link(parent, idOrganizes, n.key)
		if al.cfg.Source != "" {
			source, ok := a.nodes["ns=1;s="+al.cfg.Source]
			if !ok || source.point == nil {
				return nil, fmt.Errorf("alarm %q: no point %q", al.cfg.Name, al.cfg.Source)
			}
			al.source = source
			a.link(source.key, idHasCondition, n.key)
		}
		if err := a.alarmVariables(n, al); err != nil {
			return nil, err
		}
	}

	// Instances point to their types.
	for _, n := range a.nodes {
		if n.typeDef != 0 && (n.class == ua.NodeClass_nodeClassObject || n.class == ua.NodeClass_nodeClassVariable) {
			if _, ok := a.nodes[numericKey(n.typeDef)]; ok {
				a.link(n.key, idHasTypeDefinition, numericKey(n.typeDef))
			}
		}
	}
	return a, nil
}

// members adds a variable per field of a structure point, and per field of those that are
// structures, under the variable they belong to.
func (a *addressSpace) members(parent *node, t *structureType, path []string) error {
	for _, f := range t.fields {
		memberPath := append(append([]string{}, path...), f.name)
		key := parent.key + "." + f.name
		if _, taken := a.nodes[key]; taken {
			return fmt.Errorf("point %q: the member %s is taken", parent.point.Name, key)
		}
		name := strings.TrimPrefix(key, "ns=1;s=")
		n := a.add(&node{key: key, id: stringNodeId(1, name), class: ua.NodeClass_nodeClassVariable, namespace: 1, name: f.name,
			description: f.description, typeDef: idBaseDataVariableType, dataType: f.dataType, valueRank: -1,
			point: parent.point, path: memberPath})
		if f.arrayLength > 0 {
			n.valueRank, n.dims = 1, []uint32{uint32(f.arrayLength)}
		}
		a.link(parent.key, idHasComponent, key)
		if f.structure != nil {
			n.structure, n.dataTypeId = f.structure, f.structure.id
			if err := a.members(n, f.structure, memberPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// alarmVariables adds the variables of a condition a client reads its state by.
func (a *addressSpace) alarmVariables(n *node, al *alarm) error {
	add := func(parent *node, name string, dataType uint32, typeDef uint32, value func(st alarmState) ua.Variant) *node {
		key := parent.key + "." + name
		v := a.add(&node{key: key, id: stringNodeId(1, strings.TrimPrefix(key, "ns=1;s=")), class: ua.NodeClass_nodeClassVariable,
			name: name, typeDef: typeDef, dataType: dataType, valueRank: -1,
			variant: func() ua.Variant { return value(al.snapshot()) }})
		referenceType := uint32(idHasComponent)
		if typeDef == idPropertyType {
			referenceType = idHasProperty
		}
		a.link(parent.key, referenceType, key)
		return v
	}
	for _, name := range []string{"EnabledState", "ActiveState", "AckedState", "ConfirmedState", "Severity", "Message", "Retain",
		"ConditionName", "Comment"} {
		if _, taken := a.nodes[n.key+"."+name]; taken {
			return fmt.Errorf("alarm %q: %s.%s is taken", al.cfg.Name, al.cfg.Name, name)
		}
	}
	twoState := func(name string, get func(st alarmState) bool, yes, no string) {
		v := add(n, name, idLocalizedText, idTwoStateVariableType, func(st alarmState) ua.Variant {
			if get(st) {
				return variantText(yes)
			}
			return variantText(no)
		})
		add(v, "Id", idBoolean, idPropertyType, func(st alarmState) ua.Variant { return variantBool(get(st)) })
	}
	twoState("EnabledState", func(st alarmState) bool { return st.enabled }, "Enabled", "Disabled")
	twoState("ActiveState", func(st alarmState) bool { return st.active }, "Active", "Inactive")
	twoState("AckedState", func(st alarmState) bool { return st.acked }, "Acknowledged", "Unacknowledged")
	twoState("ConfirmedState", func(st alarmState) bool { return st.confirmed }, "Confirmed", "Unconfirmed")
	add(n, "Severity", TypeUInt16, idPropertyType, func(alarmState) ua.Variant { return variantUInt16(al.cfg.Severity) })
	add(n, "Message", idLocalizedText, idPropertyType, func(alarmState) ua.Variant { return variantText(al.cfg.Message) })
	add(n, "Retain", idBoolean, idPropertyType, func(st alarmState) ua.Variant { return variantBool(st.retain()) })
	add(n, "ConditionName", TypeString, idPropertyType, func(alarmState) ua.Variant { return variantString(al.cfg.Name) })
	add(n, "Comment", idLocalizedText, idBaseDataVariableType, func(st alarmState) ua.Variant { return variantText(st.comment) })
	return nil
}

// accessLevel is a variable's AccessLevel: CurrentRead, CurrentWrite when it is writable,
// HistoryRead when it keeps history.
func (n *node) accessLevel(writes, history bool) uint8 {
	level := uint8(1)
	if n.point != nil && n.point.Writable && writes {
		level |= 2
	}
	if history && n.path == nil && n.point != nil && n.point.Historize {
		level |= 4
	}
	return level
}
