package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/kit/datetime"
	"github.com/muktihari/fit/profile"
	"github.com/muktihari/fit/profile/basetype"
	"github.com/muktihari/fit/profile/factory"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/proto"
	garmin "github.com/se0wtf/go-garminconnect"
)

// Synthetic binary data, deliberately independent of any account recordings.
func testFIT(t *testing.T, options ...encoder.Option) []byte {
	t.Helper()
	field := func(message typedef.MesgNum, number byte, value any) proto.Field {
		return factory.CreateField(message, number).WithValue(value)
	}
	start := datetime.ToUint32(time.Unix(1786752830, 0))
	file := &proto.FIT{FileHeader: proto.FileHeader{ProtocolVersion: proto.V2}, Messages: []proto.Message{
		{Num: typedef.MesgNumFileId, Fields: []proto.Field{field(typedef.MesgNumFileId, 0, uint8(typedef.FileActivity))}},
		{Num: typedef.MesgNumDeveloperDataId, Fields: []proto.Field{
			field(typedef.MesgNumDeveloperDataId, 3, uint8(0)), field(typedef.MesgNumDeveloperDataId, 1, []byte{1, 2, 3, 4}),
		}},
		{Num: typedef.MesgNumFieldDescription, Fields: []proto.Field{
			field(typedef.MesgNumFieldDescription, 0, uint8(0)), field(typedef.MesgNumFieldDescription, 1, uint8(0)),
			field(typedef.MesgNumFieldDescription, 2, uint8(basetype.Uint16)),
			field(typedef.MesgNumFieldDescription, 3, []string{"custom_depth"}),
			field(typedef.MesgNumFieldDescription, 6, uint8(10)), field(typedef.MesgNumFieldDescription, 7, int8(2)),
			field(typedef.MesgNumFieldDescription, 8, []string{"m"}),
		}},
		{Num: typedef.MesgNumDiveSettings, Fields: []proto.Field{field(typedef.MesgNumDiveSettings, 253, start)}},
		{Num: typedef.MesgNumDiveGas, Fields: []proto.Field{field(typedef.MesgNumDiveGas, 0, uint8(0)), field(typedef.MesgNumDiveGas, 1, uint8(21))}},
		{Num: typedef.MesgNumTankUpdate, Fields: []proto.Field{
			field(typedef.MesgNumTankUpdate, 253, start), field(typedef.MesgNumTankUpdate, 0, uint32(123)), field(typedef.MesgNumTankUpdate, 1, uint16(20000)),
		}},
		{Num: typedef.MesgNumEvent, Fields: []proto.Field{
			field(typedef.MesgNumEvent, 253, start), field(typedef.MesgNumEvent, 0, uint8(typedef.EventDiveAlert)), field(typedef.MesgNumEvent, 3, uint32(0)),
		}},
		{Num: typedef.MesgNumSession, Fields: []proto.Field{
			field(typedef.MesgNumSession, 8, uint32(60000)), field(typedef.MesgNumSession, 141, uint32(22000)),
			field(typedef.MesgNumSession, 140, uint32(15000)),
		}},
	}}
	for i, depth := range []uint32{0, 1586, basetype.Uint32Invalid} {
		heartRate, temperature := uint8(80+i), int8(-i)
		if i == 2 {
			heartRate, temperature = basetype.Uint8Invalid, basetype.Sint8Invalid
		}
		file.Messages = append(file.Messages, proto.Message{Num: typedef.MesgNumRecord, Fields: []proto.Field{
			field(typedef.MesgNumRecord, 253, start+uint32(i*i)),
			field(typedef.MesgNumRecord, 92, depth), field(typedef.MesgNumRecord, 3, heartRate), field(typedef.MesgNumRecord, 13, temperature),
			field(typedef.MesgNumRecord, 93, uint32(3000)), field(typedef.MesgNumRecord, 94, uint32(60)),
			field(typedef.MesgNumRecord, 95, uint32(180)), field(typedef.MesgNumRecord, 96, uint32(90)),
			field(typedef.MesgNumRecord, 97, uint8(5)), field(typedef.MesgNumRecord, 98, uint16(20)),
			field(typedef.MesgNumRecord, 123, uint32(1200)), field(typedef.MesgNumRecord, 124, uint16(70)),
			field(typedef.MesgNumRecord, 125, uint16(2050)), field(typedef.MesgNumRecord, 126, uint16(1850)),
			field(typedef.MesgNumRecord, 127, int32(-250)), field(typedef.MesgNumRecord, 129, uint8(21)),
			{FieldBase: &proto.FieldBase{Name: "unknown", Num: 240, Type: profile.Uint64, BaseType: basetype.Uint64, Scale: 1}, Value: proto.Uint64(math.MaxUint64 - 1)},
			{FieldBase: &proto.FieldBase{Name: "unknown", Num: 241, Type: profile.Uint16, BaseType: basetype.Uint16, Scale: 1, Array: true}, Value: proto.SliceUint16([]uint16{0, basetype.Uint16Invalid, 50})},
			{FieldBase: &proto.FieldBase{Name: "unknown", Num: 242, Type: profile.Float32, BaseType: basetype.Float32, Scale: 1}, Value: proto.Float32(math.Float32frombits(basetype.Float32Invalid))},
			{FieldBase: &proto.FieldBase{Name: "unknown", Num: 243, Type: profile.Float64, BaseType: basetype.Float64, Scale: 1}, Value: proto.Float64(math.Inf(1))},
		}, DeveloperFields: []proto.DeveloperField{{Num: 0, DeveloperDataIndex: 0, Value: proto.Uint16(300)}}})
	}
	file.Messages = append(file.Messages, proto.Message{Num: typedef.MesgNum(0xff01), Fields: []proto.Field{
		{FieldBase: &proto.FieldBase{Name: "unknown", Num: 0, Type: profile.Byte, BaseType: basetype.Byte, Scale: 1, Array: true}, Value: proto.SliceUint8([]byte{1, 2, 3})},
	}})
	var body bytes.Buffer
	if err := encoder.New(&body, options...).Encode(file); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func testFITZIP(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var body bytes.Buffer
	archive := zip.NewWriter(&body)
	for name, data := range files {
		writer, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestDecodeFITPreservesAllMessagesAndProfile(t *testing.T) {
	for _, options := range [][]encoder.Option{nil, {encoder.WithBigEndian()}, {encoder.WithHeaderOption(encoder.HeaderOptionCompressedTimestamp, 3)}} {
		body := testFIT(t, options...)
		document, err := decodeFITDownload(context.Background(), body)
		if err != nil {
			t.Fatal(err)
		}
		original, err := base64.StdEncoding.DecodeString(document.Files[0].OriginalBase64)
		if err != nil || !bytes.Equal(body, original) {
			t.Fatal("original FIT bytes were not preserved")
		}
		if _, err := json.Marshal(document); err != nil {
			t.Fatalf("FIT contains values that cannot be stored as JSON: %v", err)
		}
		kinds := make(map[uint16]int)
		for _, message := range document.Files[0].Sequences[0].Messages {
			kinds[message.Number]++
			if message.Number == uint16(typedef.MesgNumRecord) {
				developer := message.DeveloperFields[0]
				if developer.Name != "custom_depth" || developer.Units != "m" || developer.Value != float64(28) {
					t.Fatalf("developer field metadata/scale missing: %#v", developer)
				}
				for _, field := range message.Fields {
					if field.Number == 240 && field.Raw != uint64(math.MaxUint64-1) {
						t.Fatal("unknown 64-bit field lost precision")
					}
					if field.Number == 241 {
						values := field.Value.([]any)
						if values[0] != uint16(0) || values[1] != nil || values[2] != uint16(50) {
							t.Fatal("invalid array elements were not decoded as null")
						}
					}
					if (field.Number == 242 || field.Number == 243) && (field.Raw != nil || field.Value != nil) {
						t.Fatal("non-finite floating point values must be null")
					}
				}
			}
		}
		for _, kind := range []uint16{uint16(typedef.MesgNumDiveSettings), uint16(typedef.MesgNumDiveGas), uint16(typedef.MesgNumTankUpdate), uint16(typedef.MesgNumEvent), 0xff01} {
			if kinds[kind] == 0 {
				t.Errorf("lost FIT message %d", kind)
			}
		}
		stored, err := withFITData(storedDive{}, document)
		if err != nil || len(stored.Samples) != 3 || len(stored.Alerts) != 1 {
			t.Fatalf("FIT profile: samples=%d alerts=%d err=%v", len(stored.Samples), len(stored.Alerts), err)
		}
		for i, elapsed := range []int64{0, 1000, 4000} {
			if stored.Samples[i].TimestampMS != 1786752830000+elapsed || stored.Samples[i].ElapsedMS != elapsed {
				t.Errorf("sample %d: timestamp=%d elapsed=%d; want %d/%d", i, stored.Samples[i].TimestampMS, stored.Samples[i].ElapsedMS, 1786752830000+elapsed, elapsed)
			}
		}
		for key, want := range map[string]float64{
			"DEPTH": 1.586, "HR": 81, "TEMPERATURE": -1, "NEXT_STOP_DEPTH": 3, "NEXT_STOP_TIME": 60,
			"TIME_TO_SURFACE": 180, "NDL_TIME": 90, "PERCENT_CNS_LOAD": 5, "PERCENT_N2_LOAD": 20,
			"PSAC": 0.7, "VSAC": 20.5, "RMV": 18.5, "AIR_TIME_REMAINING": 1200, "PO2": 0.21, "ASCENT_RATE": -0.25,
		} {
			if got := stored.Samples[1].Values[key]; got == nil || math.Abs(*got-want) > 1e-9 {
				t.Errorf("incorrect FIT metric %s: %v; want %v", key, got, want)
			}
		}
		if stored.Samples[0].Values["DEPTH"] == nil || *stored.Samples[0].Values["DEPTH"] != 0 || stored.Samples[2].Values["DEPTH"] != nil || stored.Samples[2].Values["HR"] != nil || stored.Samples[2].Values["TEMPERATURE"] != nil {
			t.Error("invalid sentinels and recorded zeroes were conflated")
		}
		if stored.MaxDepth == nil || *stored.MaxDepth != 22 || stored.Duration == nil || *stored.Duration != 60 {
			t.Error("FIT session summary was not mapped")
		}
	}
}

func TestDecodeFITContainersAndErrors(t *testing.T) {
	body := testFIT(t)
	chained := append(append([]byte{}, body...), body...)
	document, err := decodeFITDownload(context.Background(), testFITZIP(t, map[string][]byte{"nested/DIVE.FIT": chained, "other.fit": body, "readme.txt": []byte("ignored")}))
	if err != nil || len(document.Files) != 2 {
		t.Fatalf("multi-file archive: %v", err)
	}
	sequences := 0
	for _, file := range document.Files {
		sequences += len(file.Sequences)
	}
	if sequences != 3 {
		t.Fatal("chained FIT sequences were lost")
	}
	corrupt := append([]byte{}, body...)
	corrupt[len(corrupt)-1] ^= 1
	for _, invalid := range [][]byte{nil, []byte("not a FIT"), corrupt, body[:len(body)-1], append(body, []byte("trailing garbage")...), testFITZIP(t, map[string][]byte{"readme.txt": []byte("no FIT")}), testFITZIP(t, map[string][]byte{"dive.fit": corrupt})} {
		if _, err := decodeFITDownload(context.Background(), invalid); err == nil {
			t.Error("invalid FIT/archive was accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := decodeFITDownload(ctx, body); !errors.Is(err, context.Canceled) {
		t.Fatalf("FIT decoding ignored cancellation: %v", err)
	}
}

type fitDiveClient struct {
	*fakeDiveClient
	files map[int64][]byte
	err   error
	calls []int64
}

func (c *fitDiveClient) DownloadActivity(_ context.Context, id int64, format garmin.DownloadFormat) ([]byte, error) {
	c.calls = append(c.calls, id)
	if format != garmin.Original {
		return nil, errors.New("expected original FIT download")
	}
	return c.files[id], c.err
}

func TestFITBackfillAndTransactionalFailure(t *testing.T) {
	document, err := decodeFITDownload(context.Background(), testFIT(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "backfill", true: "failed backfill"}[failure], func(t *testing.T) {
			archive := newArchive()
			old := testSQLDive(t, 42)
			archive.Dives = []DiveRecord{old, {ID: 43, FIT: document}}
			client := &fitDiveClient{fakeDiveClient: &fakeDiveClient{activities: []garmin.Activity{
				{ID: 42, ActivityType: garmin.ActivityType{Key: "single_gas_diving"}},
				{ID: 43, ActivityType: garmin.ActivityType{Key: "single_gas_diving"}},
			}}, files: map[int64][]byte{42: testFIT(t)}}
			if failure {
				client.activities = append(client.activities, garmin.Activity{ID: 44, ActivityType: garmin.ActivityType{Key: "single_gas_diving"}})
				client.files[44] = []byte("corrupt FIT")
			}
			changed, err := syncDives(context.Background(), client, &archive, nil)
			if failure {
				if err == nil || changed != 0 || !reflect.DeepEqual(archive.Dives, []DiveRecord{old, {ID: 43, FIT: document}}) {
					t.Fatal("failed FIT backfill changed the archive")
				}
				return
			}
			if err != nil || changed != 1 || len(archive.Dives) != 2 || archive.Dives[1].FIT == nil || !reflect.DeepEqual(archive.Dives[1].DiveDetails, old.DiveDetails) || len(client.activityDetailCalls) != 0 || len(client.diveDetailCalls) != 0 {
				t.Fatalf("FIT backfill did not preserve existing documents: %v", err)
			}
			if changed, err := syncDives(context.Background(), client, &archive, nil); err != nil || changed != 0 || !reflect.DeepEqual(client.calls, []int64{42}) {
				t.Fatalf("FIT backfill was repeated: %v", err)
			}
			path := filepath.Join(t.TempDir(), "dives.yaml")
			if err := saveArchive(path, archive); err != nil {
				t.Fatal(err)
			}
			loaded, _, err := loadArchive(path)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Dives[1].FIT.Files[0].OriginalBase64 != archive.Dives[1].FIT.Files[0].OriginalBase64 {
				t.Fatal("YAML did not preserve original FIT")
			}
			if _, err := prepareStoredDive(loaded.Dives[1]); err != nil {
				t.Fatalf("YAML FIT cannot be used by SQL mapper: %v", err)
			}
		})
	}
}

func TestFITSQLBackfillIsAtomicAndPreservesDocuments(t *testing.T) {
	store, _ := openTestSQLStore(t)
	ctx := context.Background()
	old := testSQLDive(t, 42)
	old.DiveDetails.(map[string]any)["profile"].(map[string]any)["maxDepth"] = 33.0
	if err := store.SaveDives(ctx, []DiveRecord{old}); err != nil {
		t.Fatal(err)
	}
	document, err := decodeFITDownload(ctx, testFIT(t))
	if err != nil {
		t.Fatal(err)
	}
	update := DiveRecord{ID: 42, FIT: document}
	invalid := DiveRecord{ID: 43, FIT: &fitDocument{Version: 99}}
	if err := store.SaveDives(ctx, []DiveRecord{update, invalid}); err == nil {
		t.Fatal("invalid FIT backfill was accepted")
	}
	before, err := store.Dive(ctx, 42)
	if err != nil || string(before.FIT) != "null" {
		t.Fatalf("failed FIT backfill did not roll back: %v", err)
	}
	if files, err := store.FITFiles(ctx, 42); err != nil || len(files) != 0 {
		t.Fatalf("failed FIT backfill retained original file BLOBs: %v", err)
	}
	if err := store.SaveDives(ctx, []DiveRecord{update, update}); err != nil {
		t.Fatal(err)
	}
	after, err := store.Dive(ctx, 42)
	if err != nil || !bytes.Equal(before.DiveDetails, after.DiveDetails) || before.Dive.Name != after.Dive.Name || !reflect.DeepEqual(before.Dive.MaxDepthMeters, after.Dive.MaxDepthMeters) || !json.Valid(after.FIT) || !strings.Contains(string(after.FIT), "original_base64") {
		t.Fatalf("FIT enrichment overwrote the stored summary/documents: %v", err)
	}
	samples, _, err := store.DiveSamples(ctx, 42, -1, 10)
	if err != nil || len(samples) != 3 || samples[1].DepthMeters == nil || *samples[1].DepthMeters != 1.586 {
		t.Fatalf("FIT backfill did not replace/index profile samples: %v", err)
	}
	state, err := store.DiveSyncState(ctx)
	if err != nil || len(state) != 1 || !state[0].fitComplete {
		t.Fatalf("FIT backfill completion not stored: %v", err)
	}
	if files, err := store.FITFiles(ctx, 42); err != nil || len(files) != 1 {
		t.Fatalf("FIT backfill did not persist the original file BLOB: %v", err)
	}
}

func TestFITBackfillThroughAppDestinations(t *testing.T) {
	for _, kind := range []string{outputYAML, outputSQLite} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "dives")
			config := outputConfig{Type: kind, Endpoint: path}
			original := testSQLDive(t, 42)
			archive := newArchive()
			archive.Dives = []DiveRecord{original}
			destination, err := openDestination(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			if err := destination.Save(ctx, archive, 1); err != nil {
				t.Fatal(err)
			}
			if err := destination.Close(); err != nil {
				t.Fatal(err)
			}
			client := &fitDiveClient{fakeDiveClient: &fakeDiveClient{activities: []garmin.Activity{
				{ID: 42, ActivityType: garmin.ActivityType{Key: "single_gas_diving"}},
			}}, files: map[int64][]byte{42: testFIT(t)}}
			dependencies := appDependencies{
				configDirectory: func() (string, error) { return t.TempDir(), nil },
				openDestination: openDestination,
				loadCached:      func(string) (diveClient, error) { return client, nil },
			}
			args := []string{"-output", path}
			if kind == outputSQLite {
				args = []string{"-output-type", kind, "-output-dsn", path}
			}
			for i := 0; i < 2; i++ {
				if err := runWithDependencies(ctx, args, &bytes.Buffer{}, testLogger(t), dependencies); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(client.calls, []int64{42}) || len(client.diveDetailCalls) != 0 || len(client.activityDetailCalls) != 0 {
				t.Fatal("FIT backfill re-fetched existing data or repeated a completed download")
			}
			if kind == outputSQLite {
				store, err := openSQLStore(ctx, kind, path)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				samples, _, err := store.DiveSamples(ctx, 42, -1, 10)
				if err != nil || len(samples) != 3 {
					t.Fatalf("app did not persist FIT backfill samples: %v", err)
				}
				if files, err := store.FITFiles(ctx, 42); err != nil || len(files) != 1 {
					t.Fatalf("app did not persist the original FIT BLOB: %v", err)
				}
			}
		})
	}
}
