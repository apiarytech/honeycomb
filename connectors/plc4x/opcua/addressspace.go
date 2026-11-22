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
	idRoot                  = 84
	idObjects               = 85
	idTypes                 = 86
	idViews                 = 87
	idReferences            = 31
	idNonHierarchical       = 32
	idHierarchical          = 33
	idHasChild              = 34
	idOrganizes             = 35
	idHasTypeDefinition     = 40
	idAggregates            = 44
	idHasSubtype            = 45
	idHasProperty           = 46
	idHasComponent          = 47
	idBaseObjectType        = 58
	idFolderType            = 61
	idBaseDataVariableType  = 63
	idPropertyType          = 68
	idServer                = 2253
	idServerArray           = 2254
	idNamespaceArray        = 2255
	idServerStatus          = 2256
	idServerStatusStartTime = 2257
	idServerCurrentTime     = 2258
	idServerState           = 2259
	idServerType            = 2004
	idServerStatusType      = 2138
	idServerStateType       = 852
	idServerStatusDataType  = 862
)

// superTypes are the reference types' supertypes, for a Browse with subtypes.
var superTypes = map[uint32]uint32{
	idNonHierarchical: idReferences, idHierarchical: idReferences, idHasChild: idHierarchical,
	idOrganizes: idHierarchical, idAggregates: idHasChild, idHasSubtype: idHasChild,
	idHasProperty: idAggregates, idHasComponent: idAggregates, idHasTypeDefinition: idNonHierarchical,
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

// Attribute ids (OPC UA Part 6, A.1).
const (
	attrNodeId                  = 1
	attrNodeClass               = 2
	attrBrowseName              = 3
	attrDisplayName             = 4
	attrDescription             = 5
	attrWriteMask               = 6
	attrUserWriteMask           = 7
	attrEventNotifier           = 12
	attrValue                   = 13
	attrDataType                = 14
	attrValueRank               = 15
	attrArrayDimensions         = 16
	attrAccessLevel             = 17
	attrUserAccessLevel         = 18
	attrMinimumSamplingInterval = 19
	attrHistorizing             = 20
)

// reference is one reference of a node.
type reference struct {
	typeId  uint32 // in namespace 0
	target  string // the target's key
	forward bool
}

// node is a node of the address space. Variables are a bound point, or one of the server's
// own (value returns their value).
type node struct {
	key         string
	id          ua.NodeId
	class       ua.NodeClass
	namespace   uint16 // of the browse name
	name        string
	description string
	typeDef     uint32
	references  []reference

	dataType  uint32
	valueRank int32
	dims      []uint32
	point     *Point
	value     func() (any, uint32) // a server variable: its value and data type
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

// addressSpace is the server's nodes: the standard ones a client expects (Root, Objects,
// Server with its status and namespaces) and, under Objects, a folder per dotted prefix of
// the points' names with the points as variables.
type addressSpace struct {
	nodes map[string]*node
}

// newAddressSpace builds the nodes. namespaces are the server's namespace URIs (index 1 is the
// points'); status gives the ServerStatus variables their values.
func newAddressSpace(points []Point, namespaces []string, status func(id uint32) (any, uint32)) (*addressSpace, error) {
	a := &addressSpace{nodes: map[string]*node{}}
	add := func(n *node) *node {
		a.nodes[n.key] = n
		return n
	}
	standard := func(id uint32, class ua.NodeClass, name string, typeDef uint32) *node {
		return add(&node{key: numericKey(id), id: numericNodeId(0, id), class: class, name: name, typeDef: typeDef})
	}
	standard(idRoot, ua.NodeClass_nodeClassObject, "Root", idFolderType)
	standard(idObjects, ua.NodeClass_nodeClassObject, "Objects", idFolderType)
	standard(idTypes, ua.NodeClass_nodeClassObject, "Types", idFolderType)
	standard(idViews, ua.NodeClass_nodeClassObject, "Views", idFolderType)
	standard(idServer, ua.NodeClass_nodeClassObject, "Server", idServerType)
	serverStatus := standard(idServerStatus, ua.NodeClass_nodeClassVariable, "ServerStatus", idServerStatusType)
	serverStatus.dataType, serverStatus.valueRank = idServerStatusDataType, -1
	a.link(numericKey(idRoot), idOrganizes, numericKey(idObjects))
	a.link(numericKey(idRoot), idOrganizes, numericKey(idTypes))
	a.link(numericKey(idRoot), idOrganizes, numericKey(idViews))
	a.link(numericKey(idObjects), idOrganizes, numericKey(idServer))
	a.link(numericKey(idServer), idHasComponent, numericKey(idServerStatus))

	serverVariable := func(id uint32, parent uint32, name string, dataType uint32, valueRank int32, typeDef, referenceType uint32) {
		n := standard(id, ua.NodeClass_nodeClassVariable, name, typeDef)
		n.dataType, n.valueRank = dataType, valueRank
		n.value = func() (any, uint32) { return status(id) }
		a.link(numericKey(parent), referenceType, n.key)
	}
	serverVariable(idNamespaceArray, idServer, "NamespaceArray", TypeString, 1, idPropertyType, idHasProperty)
	serverVariable(idServerArray, idServer, "ServerArray", TypeString, 1, idPropertyType, idHasProperty)
	serverVariable(idServerStatusStartTime, idServerStatus, "StartTime", TypeDateTime, -1, idBaseDataVariableType, idHasComponent)
	serverVariable(idServerCurrentTime, idServerStatus, "CurrentTime", TypeDateTime, -1, idBaseDataVariableType, idHasComponent)
	serverVariable(idServerState, idServerStatus, "State", idServerStateType, -1, idBaseDataVariableType, idHasComponent)

	for i := range points {
		p := &points[i]
		if p.Name == "" || strings.HasPrefix(p.Name, ".") || strings.HasSuffix(p.Name, ".") || strings.Contains(p.Name, "..") {
			return nil, fmt.Errorf("point %q: a name is dot-separated parts, none empty", p.Name)
		}
		parts := strings.Split(p.Name, ".")
		parent := numericKey(idObjects)
		for depth := 1; depth < len(parts); depth++ {
			folderName := strings.Join(parts[:depth], ".")
			key := fmt.Sprintf("ns=1;s=%s", folderName)
			folder, exists := a.nodes[key]
			if !exists {
				folder = add(&node{key: key, id: stringNodeId(1, folderName), class: ua.NodeClass_nodeClassObject,
					namespace: 1, name: parts[depth-1], typeDef: idFolderType})
				a.link(parent, idOrganizes, key)
			} else if folder.class != ua.NodeClass_nodeClassObject {
				return nil, fmt.Errorf("point %q: %s is a point, it cannot also be a folder", p.Name, folderName)
			}
			parent = key
		}
		key := fmt.Sprintf("ns=1;s=%s", p.Name)
		if _, exists := a.nodes[key]; exists {
			return nil, fmt.Errorf("point %q: the name is taken", p.Name)
		}
		n := add(&node{key: key, id: stringNodeId(1, p.Name), class: ua.NodeClass_nodeClassVariable, namespace: 1,
			name: parts[len(parts)-1], description: p.Description, typeDef: idBaseDataVariableType,
			dataType: p.DataType, valueRank: -1, point: p})
		if p.ArrayLength > 0 {
			n.valueRank, n.dims = 1, []uint32{uint32(p.ArrayLength)}
		}
		a.link(parent, idOrganizes, key)
	}
	return a, nil
}

// link adds a reference and its inverse.
func (a *addressSpace) link(source string, referenceType uint32, target string) {
	a.nodes[source].references = append(a.nodes[source].references, reference{typeId: referenceType, target: target, forward: true})
	a.nodes[target].references = append(a.nodes[target].references, reference{typeId: referenceType, target: source, forward: false})
}

// accessLevel is a variable's AccessLevel: CurrentRead, and CurrentWrite when it is writable.
func (n *node) accessLevel(writes bool) uint8 {
	if n.point != nil && n.point.Writable && writes {
		return 3
	}
	return 1
}
