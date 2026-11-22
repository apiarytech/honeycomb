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
	"time"

	ua "github.com/apache/plc4x/plc4go/protocols/opcua/readwrite/model"
)

// Scalar Variants of the types events and the server's own variables use.

func variantNull() ua.Variant { return ua.NewVariantNull(false, false, nil, nil) }

func variantBool(b bool) ua.Variant {
	v := byte(0)
	if b {
		v = 1
	}
	return ua.NewVariantBoolean(false, false, nil, nil, nil, []byte{v})
}

func variantUInt16(v uint16) ua.Variant {
	return ua.NewVariantUInt16(false, false, nil, nil, nil, []uint16{v})
}

func variantString(s string) ua.Variant {
	return ua.NewVariantString(false, false, nil, nil, nil, []ua.PascalString{str(s)})
}

func variantText(s string) ua.Variant {
	return ua.NewVariantLocalizedText(false, false, nil, nil, nil, []ua.LocalizedText{text(s)})
}

func variantNodeId(id ua.NodeId) ua.Variant {
	return ua.NewVariantNodeId(false, false, nil, nil, nil, []ua.NodeId{id})
}

func variantByteString(b []byte) ua.Variant {
	return ua.NewVariantByteString(false, false, nil, nil, nil, []ua.ByteStringArray{ua.NewByteStringArray(int32(len(b)), b)})
}

func variantDateTime(t time.Time) ua.Variant {
	return ua.NewVariantDateTime(false, false, nil, nil, nil, []int64{dateTime(t)})
}

func variantStatusCode(status uint32) ua.Variant {
	return ua.NewVariantStatusCode(false, false, nil, nil, nil, []ua.StatusCode{ua.NewStatusCode(status)})
}

// extensionObject wraps a structure the model knows, under its binary encoding id.
func extensionObject(definition ua.ExtensionObjectDefinition, encodingId uint32) ua.ExtensionObject {
	return ua.NewBinaryExtensionObjectWithMask(expandedId(int32(encodingId)), ua.NewExtensionObjectEncodingMask(false, false, true), definition)
}

// argumentsVariant is the value of an InputArguments or OutputArguments property.
func argumentsVariant(args []Argument) ua.Variant {
	objects := make([]ua.ExtensionObject, len(args))
	for i, a := range args {
		valueRank, dims := int32(-1), []uint32(nil)
		if a.ArrayLength > 0 {
			valueRank, dims = 1, []uint32{uint32(a.ArrayLength)}
		}
		objects[i] = extensionObject(ua.NewArgument(str(a.Name), numericNodeId(0, a.DataType), valueRank, dims, text(a.Description)), idArgumentBinary)
	}
	n := int32(len(objects))
	return ua.NewVariantExtensionObject(true, false, nil, nil, &n, objects)
}

// byteStringOf is the bytes of a ByteString Variant.
func byteStringOf(v ua.Variant) ([]byte, bool) {
	b, ok := v.(ua.VariantByteString)
	if !ok || len(b.GetValue()) != 1 {
		return nil, false
	}
	return b.GetValue()[0].GetValue(), true
}

// textOf is the text of a LocalizedText (or String) Variant.
func textOf(v ua.Variant) (string, bool) {
	switch t := v.(type) {
	case ua.VariantLocalizedText:
		if len(t.GetValue()) == 1 {
			return stringOf(t.GetValue()[0].GetText()), true
		}
	case ua.VariantString:
		if len(t.GetValue()) == 1 {
			return stringOf(t.GetValue()[0]), true
		}
	case ua.VariantNull:
		return "", true
	}
	return "", false
}
