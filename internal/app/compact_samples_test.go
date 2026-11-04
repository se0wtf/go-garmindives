package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSamplesOmitRawAndPreserveMeasurements(t *testing.T) {
	store, _ := openTestSQLStore(t)
	exerciseCompactSamples(t, store)
}

func exerciseCompactSamples(t *testing.T, store *sqlStore) {
	t.Helper()
	if err := store.SaveDives(context.Background(), []DiveRecord{testSQLDive(t, 950)}); err != nil {
		t.Fatal(err)
	}
	privateRaw, err := json.Marshal(map[string]string{"private": strings.Repeat("private-sample-marker", 8192)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), store.bind(`UPDATE dive_samples SET raw_json=?,
		temperature_celsius=CASE WHEN sample_index=0 THEN 0 ELSE NULL END WHERE dive_id=?`), string(privateRaw), 950); err != nil {
		t.Fatal(err)
	}
	handler := newAPIHandler(store, testLogger(t))
	cursor := ""
	for index, depth := range []float64{1.586, 2.5} {
		path := "/api/v1/dives/950/samples?limit=1&include_raw=true&cursor=" + url.QueryEscape(cursor)
		response := mediaRequest(t, handler, "GET", path, nil, "")
		if response.Code != http.StatusOK || response.Body.Len() > 4096 || bytes.Contains(response.Body.Bytes(), []byte("private-sample-marker")) {
			t.Fatalf("sample response leaked raw records: status=%d bytes=%d", response.Code, response.Body.Len())
		}
		var page sampleListResponse
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("page %d returned %d samples", index, len(page.Items))
		}
		sample := page.Items[0]
		if sample.Index != index || sample.ElapsedMS != int64(index*1000) || sample.DepthMeters == nil || *sample.DepthMeters != depth || sample.HeartRate == nil || *sample.HeartRate != float64(76+index) {
			t.Fatalf("sample measurements/order changed: %+v", sample)
		}
		if index == 0 && (sample.TemperatureCelsius == nil || *sample.TemperatureCelsius != 0) || index == 1 && sample.TemperatureCelsius != nil {
			t.Fatal("zero/missing temperature distinction was lost")
		}
		var payload struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if _, exists := payload.Items[0]["raw"]; exists {
			t.Fatal("raw must be omitted entirely, including with include_raw=true")
		}
		cursor = page.NextCursor
		if index == 0 && cursor == "" || index == 1 && cursor != "" {
			t.Fatalf("pagination changed on sample %d", index)
		}
	}
	var retained int
	if err := store.db.QueryRowContext(context.Background(), store.bind(`SELECT COUNT(*) FROM dive_samples WHERE dive_id=? AND raw_json=?`), 950, string(privateRaw)).Scan(&retained); err != nil || retained != 2 {
		t.Fatalf("internal raw samples were modified or lost: %d %v", retained, err)
	}
}
