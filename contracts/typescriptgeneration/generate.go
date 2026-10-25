// Package typescriptgeneration generates the public TypeScript contract types and their
// validation schema from the compiled module contract (sdk.md S2, UX U4). The output
// describes the host JSON form, which is protojson with its default options:
//
//   - field names are the protobuf JSON names (lowerCamelCase);
//   - a field at its default value is omitted, so every property is optional and the
//     validator fills defaults back in;
//   - enums are their value names as strings;
//   - 32-bit integers are numbers, 64-bit integers are decimal strings, bytes are base64.
//
// The generated file is contracts/typescript/src/contract.ts. A test fails when it
// differs from Generate, so it cannot drift from the .proto files. Consumers that keep
// their own copy (the kernel kit) run the command in ./command with -output.
package typescriptgeneration

//go:generate go run ./command -output ../typescript/src/contract.ts

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"

	modulerpc "github.com/mycelpf/mycel_module_sdk/contracts/modulerpc/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Files are the contract files the generator reads, in output order.
func Files() []protoreflect.FileDescriptor {
	return []protoreflect.FileDescriptor{
		modulerpc.File_contracts_modulerpc_v1_ui_contract_proto,
		modulerpc.File_contracts_modulerpc_v1_modulerpc_proto,
	}
}

// typeName is the TypeScript name of a message or enum: its name inside the package,
// with nesting joined by an underscore (HealthResponse_Status), as protoc-gen-go does.
func typeName(descriptor protoreflect.Descriptor) string {
	full := string(descriptor.FullName())
	packageName := string(descriptor.ParentFile().Package())
	return strings.ReplaceAll(strings.TrimPrefix(full, packageName+"."), ".", "_")
}

type collected struct {
	enums    []protoreflect.EnumDescriptor
	messages []protoreflect.MessageDescriptor
}

func (c *collected) addMessages(messages protoreflect.MessageDescriptors) {
	for index := 0; index < messages.Len(); index++ {
		message := messages.Get(index)
		if message.IsMapEntry() {
			continue
		}
		c.messages = append(c.messages, message)
		for enumIndex := 0; enumIndex < message.Enums().Len(); enumIndex++ {
			c.enums = append(c.enums, message.Enums().Get(enumIndex))
		}
		c.addMessages(message.Messages())
	}
}

func collect() collected {
	var c collected
	for _, file := range Files() {
		for index := 0; index < file.Enums().Len(); index++ {
			c.enums = append(c.enums, file.Enums().Get(index))
		}
		c.addMessages(file.Messages())
	}
	return c
}

// kind is the schema kind of a scalar, enum or message field.
func kind(field protoreflect.FieldDescriptor) string {
	switch field.Kind() {
	case protoreflect.BoolKind:
		return "bool"
	case protoreflect.StringKind:
		return "string"
	case protoreflect.BytesKind:
		return "bytes"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return "int32"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "uint32"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "int64"
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "uint64"
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return "double"
	case protoreflect.EnumKind:
		return "enum"
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return "message"
	}
	panic(fmt.Sprintf("typescriptgeneration: unsupported field kind %v", field.Kind()))
}

func scalarType(field protoreflect.FieldDescriptor) string {
	switch kind(field) {
	case "bool":
		return "boolean"
	case "int32", "uint32", "double":
		return "number"
	case "enum":
		return typeName(field.Enum())
	case "message":
		return typeName(field.Message())
	}
	return "string" // string, bytes (base64), int64 and uint64 (decimal)
}

func fieldType(field protoreflect.FieldDescriptor) string {
	switch {
	case field.IsMap():
		return "Record<string, " + scalarType(field.MapValue()) + ">"
	case field.IsList():
		return scalarType(field) + "[]"
	}
	return scalarType(field)
}

// fieldSchema is the validation entry of one field.
func fieldSchema(field protoreflect.FieldDescriptor) string {
	parts := []string{"json: " + strconv.Quote(field.JSONName())}
	target := field
	switch {
	case field.IsMap():
		parts = append(parts, `shape: "map"`)
		target = field.MapValue()
	case field.IsList():
		parts = append(parts, `shape: "list"`)
	case field.HasPresence():
		parts = append(parts, `shape: "optional"`)
	default:
		parts = append(parts, `shape: "single"`)
	}
	parts = append(parts, "kind: "+strconv.Quote(kind(target)))
	switch kind(target) {
	case "enum":
		parts = append(parts, "type: "+strconv.Quote(typeName(target.Enum())))
	case "message":
		parts = append(parts, "type: "+strconv.Quote(typeName(target.Message())))
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// Generate returns the TypeScript source of contract.ts.
func Generate() []byte {
	c := collect()
	var out bytes.Buffer
	write := func(format string, arguments ...any) { fmt.Fprintf(&out, format, arguments...) }

	write("// Code generated by contracts/typescriptgeneration. DO NOT EDIT.\n")
	write("// Source:")
	for _, file := range Files() {
		write(" %s", file.Path())
	}
	write("\n//\n// The host JSON form of the module contract (sdk.md S2): protojson with default options.\n")
	write("// Field names are JSON names; a field at its default value is omitted, so every property\n")
	write("// is optional; enums are value names; 64-bit integers are decimal strings; bytes are base64.\n")
	write("// Validate untrusted JSON with validate() from ./validate.ts.\n\n")
	write("export const CONTRACT_PACKAGE = %q;\n\n", modulerpc.File_contracts_modulerpc_v1_modulerpc_proto.Package())

	for _, enum := range c.enums {
		name := typeName(enum)
		var values []string
		for index := 0; index < enum.Values().Len(); index++ {
			values = append(values, strconv.Quote(string(enum.Values().Get(index).Name())))
		}
		write("export type %s =\n\t| %s;\n\n", name, strings.Join(values, "\n\t| "))
	}

	for _, message := range c.messages {
		name := typeName(message)
		fields := message.Fields()
		if fields.Len() == 0 {
			write("export type %s = Record<string, never>;\n\n", name)
			continue
		}
		write("export interface %s {\n", name)
		for index := 0; index < fields.Len(); index++ {
			field := fields.Get(index)
			write("\t%s?: %s;\n", field.JSONName(), fieldType(field))
		}
		write("}\n\n")
	}

	write("/** Every message type by name, for validate(). */\nexport interface ContractMessages {\n")
	for _, message := range c.messages {
		write("\t%s: %s;\n", typeName(message), typeName(message))
	}
	write("}\n\nexport type ContractMessageName = keyof ContractMessages;\n\n")

	write("export type FieldKind = 'bool' | 'string' | 'bytes' | 'int32' | 'uint32' | 'int64' | 'uint64' | 'double' | 'enum' | 'message';\n")
	write("export type FieldShape = 'single' | 'optional' | 'list' | 'map';\n\n")
	write("export interface FieldSchema {\n\treadonly json: string;\n\treadonly shape: FieldShape;\n\treadonly kind: FieldKind;\n\treadonly type?: string;\n}\n\n")
	write("export interface ContractSchema {\n\treadonly enums: Readonly<Record<string, readonly string[]>>;\n\treadonly messages: Readonly<Record<ContractMessageName, readonly FieldSchema[]>>;\n}\n\n")

	write("export const CONTRACT_SCHEMA: ContractSchema = {\n\tenums: {\n")
	enumNames := make([]string, 0, len(c.enums))
	for _, enum := range c.enums {
		enumNames = append(enumNames, typeName(enum))
	}
	sort.Strings(enumNames)
	enumsByName := map[string]protoreflect.EnumDescriptor{}
	for _, enum := range c.enums {
		enumsByName[typeName(enum)] = enum
	}
	for _, name := range enumNames {
		enum := enumsByName[name]
		var values []string
		for index := 0; index < enum.Values().Len(); index++ {
			values = append(values, strconv.Quote(string(enum.Values().Get(index).Name())))
		}
		write("\t\t%s: [%s],\n", name, strings.Join(values, ", "))
	}
	write("\t},\n\tmessages: {\n")
	for _, message := range c.messages {
		fields := message.Fields()
		if fields.Len() == 0 {
			write("\t\t%s: [],\n", typeName(message))
			continue
		}
		write("\t\t%s: [\n", typeName(message))
		for index := 0; index < fields.Len(); index++ {
			write("\t\t\t%s,\n", fieldSchema(fields.Get(index)))
		}
		write("\t\t],\n")
	}
	write("\t},\n};\n")
	return out.Bytes()
}
