package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/muktihari/fit/decoder"
	"github.com/muktihari/fit/kit/datetime"
	"github.com/muktihari/fit/profile"
	"github.com/muktihari/fit/profile/basetype"
	"github.com/muktihari/fit/profile/factory"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/proto"
)

const (
	fitDocumentVersion = 1
	maxFITBytes        = 128 << 20
	maxFITFiles        = 64
)

// The original bytes make unknown fields and future profile revisions
// recoverable. Every message is retained, not only the graph's record messages.
type fitDocument struct {
	Version int       `json:"version" yaml:"version"`
	Files   []fitFile `json:"files" yaml:"files"`
}

type fitFile struct {
	Name           string        `json:"name" yaml:"name"`
	OriginalBase64 string        `json:"original_base64" yaml:"original_base64"`
	Sequences      []fitSequence `json:"sequences" yaml:"sequences"`
}

type fitSequence struct {
	ProtocolVersion byte         `json:"protocol_version" yaml:"protocol_version"`
	ProfileVersion  uint16       `json:"profile_version" yaml:"profile_version"`
	Messages        []fitMessage `json:"messages" yaml:"messages"`
}

type fitMessage struct {
	Number          uint16     `json:"number" yaml:"number"`
	Name            string     `json:"name" yaml:"name"`
	TimestampMS     *int64     `json:"timestamp_ms,omitempty" yaml:"timestamp_ms,omitempty"`
	Fields          []fitField `json:"fields" yaml:"fields"`
	DeveloperFields []fitField `json:"developer_fields,omitempty" yaml:"developer_fields,omitempty"`
}

type fitField struct {
	Number             byte    `json:"number" yaml:"number"`
	Name               string  `json:"name" yaml:"name"`
	Type               string  `json:"type" yaml:"type"`
	BaseType           string  `json:"base_type" yaml:"base_type"`
	Units              string  `json:"units,omitempty" yaml:"units,omitempty"`
	Scale              float64 `json:"scale" yaml:"scale"`
	Offset             float64 `json:"offset" yaml:"offset"`
	Raw                any     `json:"raw" yaml:"raw"`
	Value              any     `json:"value" yaml:"value"`
	DeveloperDataIndex *byte   `json:"developer_data_index,omitempty" yaml:"developer_data_index,omitempty"`
	Expanded           bool    `json:"expanded,omitempty" yaml:"expanded,omitempty"`
}

func decodeFITDownload(ctx context.Context, body []byte) (*fitDocument, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(body) > maxFITBytes {
		return nil, errors.New("FIT download exceeds 128 MiB")
	}
	result := &fitDocument{Version: fitDocumentVersion}
	if len(body) >= 12 && string(body[8:12]) == ".FIT" {
		file, err := decodeFITFile(ctx, "activity.fit", body)
		if err != nil {
			return nil, err
		}
		result.Files = append(result.Files, file)
		return result, nil
	}
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, fmt.Errorf("open original FIT archive: %w", err)
	}
	remaining := int64(maxFITBytes)
	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(filepath.Ext(entry.Name), ".fit") {
			continue
		}
		if len(result.Files) >= maxFITFiles || entry.UncompressedSize64 > uint64(remaining) {
			return nil, errors.New("original FIT archive exceeds file or size limit")
		}
		reader, err := entry.Open()
		if err != nil {
			return nil, fmt.Errorf("open archived FIT: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, remaining+1))
		closeErr := reader.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read archived FIT: %w", readErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if int64(len(data)) > remaining {
			return nil, errors.New("original FIT archive exceeds 128 MiB")
		}
		remaining -= int64(len(data))
		file, err := decodeFITFile(ctx, entry.Name, data)
		if err != nil {
			return nil, err
		}
		result.Files = append(result.Files, file)
	}
	if len(result.Files) == 0 {
		return nil, errors.New("original activity archive contains no FIT files")
	}
	return result, nil
}

func decodeFITFile(ctx context.Context, name string, data []byte) (fitFile, error) {
	result := fitFile{Name: name, OriginalBase64: base64.StdEncoding.EncodeToString(data)}
	dec := decoder.New(bytes.NewReader(data)) // CRC validation and component expansion are enabled.
	for consumed := 0; consumed < len(data); {
		decoded, err := dec.DecodeWithContext(ctx)
		if err != nil {
			return fitFile{}, fmt.Errorf("decode original FIT: %w", err)
		}
		consumed += int(decoded.FileHeader.Size) + int(decoded.FileHeader.DataSize) + 2
		sequence := fitSequence{
			ProtocolVersion: byte(decoded.FileHeader.ProtocolVersion),
			ProfileVersion:  decoded.FileHeader.ProfileVersion,
			Messages:        make([]fitMessage, 0, len(decoded.Messages)),
		}
		descriptions := make(map[[2]byte]proto.FieldBase)
		for i := range decoded.Messages {
			if err := ctx.Err(); err != nil {
				return fitFile{}, err
			}
			message := &decoded.Messages[i]
			if message.Num == typedef.MesgNumFieldDescription {
				rememberDeveloperField(descriptions, message)
			}
			stored := fitMessage{Number: uint16(message.Num), Name: message.Num.String(), Fields: make([]fitField, 0, len(message.Fields))}
			for _, field := range message.Fields {
				// Dynamic fields (for example event.data) depend on other fields.
				if sub := field.SubFieldSubstitution(message); sub != nil {
					base := *field.FieldBase
					base.Name, base.Type, base.Scale, base.Offset, base.Units = sub.Name, sub.Type, sub.Scale, sub.Offset, sub.Units
					field.FieldBase = &base
				}
				stored.Fields = append(stored.Fields, storeFITField(field))
				if field.Name == "timestamp" && field.Value.Valid(field.BaseType) && field.Value.Type() == proto.TypeUint32 {
					timestamp := datetime.ToTime(field.Value.Uint32()).UnixMilli()
					stored.TimestampMS = &timestamp
				}
			}
			for _, field := range message.DeveloperFields {
				base, found := descriptions[[2]byte{field.DeveloperDataIndex, field.Num}]
				if !found {
					base = proto.FieldBase{Name: "unknown", Num: field.Num, BaseType: basetype.Byte, Scale: 1}
				}
				storedField := storeFITField(proto.Field{FieldBase: &base, Value: field.Value})
				index := field.DeveloperDataIndex
				storedField.DeveloperDataIndex = &index
				stored.DeveloperFields = append(stored.DeveloperFields, storedField)
			}
			sequence.Messages = append(sequence.Messages, stored)
		}
		result.Sequences = append(result.Sequences, sequence)
	}
	if len(result.Sequences) == 0 {
		return fitFile{}, errors.New("original FIT file contains no FIT sequences")
	}
	return result, nil
}

func rememberDeveloperField(descriptions map[[2]byte]proto.FieldBase, message *proto.Message) {
	index := message.FieldByNum(0)
	number := message.FieldByNum(1)
	baseType := message.FieldByNum(2)
	if index == nil || number == nil || baseType == nil || !index.Value.Valid(index.BaseType) || !number.Value.Valid(number.BaseType) {
		return
	}
	base := proto.FieldBase{Name: "unknown", Num: number.Value.Uint8(), BaseType: basetype.BaseType(baseType.Value.Uint8()), Scale: 1}
	base.Type = profile.ProfileTypeFromString(base.BaseType.String())
	if nativeMessage, nativeField := message.FieldByNum(14), message.FieldByNum(15); nativeMessage != nil && nativeField != nil && nativeMessage.Value.Valid(nativeMessage.BaseType) && nativeField.Value.Valid(nativeField.BaseType) {
		native := factory.CreateField(typedef.MesgNum(nativeMessage.Value.Uint16()), nativeField.Value.Uint8())
		base.Name, base.Type, base.Scale, base.Offset, base.Units = native.Name, native.Type, native.Scale, native.Offset, native.Units
	}
	if f := message.FieldByNum(3); f != nil {
		base.Name = fitText(f.Value)
	}
	if f := message.FieldByNum(6); f != nil && f.Value.Valid(f.BaseType) && f.Value.Uint8() != 0 {
		base.Scale = float64(f.Value.Uint8())
	}
	if f := message.FieldByNum(7); f != nil && f.Value.Valid(f.BaseType) {
		base.Offset = float64(f.Value.Int8())
	}
	if f := message.FieldByNum(8); f != nil {
		base.Units = fitText(f.Value)
	}
	descriptions[[2]byte{index.Value.Uint8(), base.Num}] = base
}

func fitText(value proto.Value) string {
	if value.Type() == proto.TypeSliceString {
		return strings.TrimRight(strings.Join(value.SliceString(), ","), "\x00")
	}
	return strings.TrimRight(value.String(), "\x00")
}

func storeFITField(field proto.Field) fitField {
	scale := field.Scale
	if scale == 0 {
		scale = 1
	}
	return fitField{
		Number: field.Num, Name: field.Name, Type: field.Type.String(), BaseType: field.BaseType.String(),
		Units: field.Units, Scale: scale, Offset: field.Offset,
		Expanded: field.IsExpandedField,
		Raw:      fitValue(field.Value.Any(), field.BaseType, 1, 0, false),
		Value:    fitValue(field.Value.Any(), field.BaseType, scale, field.Offset, true),
	}
}

// Arrays are handled element by element so invalid sentinels become null while
// legitimate zeroes survive. Raw integers retain their full 64-bit precision.
func fitValue(value any, baseType basetype.BaseType, scale, offset float64, validate bool) any {
	if value == nil {
		return nil
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() == reflect.Slice {
		result := make([]any, rv.Len())
		for i := range result {
			result[i] = fitValue(rv.Index(i).Interface(), baseType, scale, offset, validate)
		}
		return result
	}
	if (rv.Kind() == reflect.Float32 || rv.Kind() == reflect.Float64) && (math.IsNaN(rv.Float()) || math.IsInf(rv.Float(), 0)) {
		return nil
	}
	if validate && !proto.Any(value).Valid(baseType) {
		return nil
	}
	if number := fitNumber(value); number != nil {
		if scale != 1 || offset != 0 {
			return *number/scale - offset
		}
	}
	return value
}

func fitNumber(value any) *float64 {
	if value == nil {
		return nil
	}
	rv := reflect.ValueOf(value)
	var number float64
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		number = float64(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		number = float64(rv.Uint())
	case reflect.Float32, reflect.Float64:
		number = rv.Float()
	default:
		return nil
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return nil
	}
	return &number
}
