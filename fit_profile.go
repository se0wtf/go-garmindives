package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/muktihari/fit/profile/typedef"
)

var fitSampleKeys = map[string]string{
	"heart_rate": "HR", "temperature": "TEMPERATURE", "depth": "DEPTH",
	"next_stop_depth": "NEXT_STOP_DEPTH", "next_stop_time": "NEXT_STOP_TIME",
	"time_to_surface": "TIME_TO_SURFACE", "ndl_time": "NDL_TIME",
	"cns_load": "PERCENT_CNS_LOAD", "n2_load": "PERCENT_N2_LOAD",
	"pressure_sac": "PSAC", "volume_sac": "VSAC", "rmv": "RMV",
	"air_time_remaining": "AIR_TIME_REMAINING", "po2": "PO2", "ascent_rate": "ASCENT_RATE",
}

func withFITData(dive storedDive, document *fitDocument) (storedDive, error) {
	encoded, err := json.Marshal(document)
	if err != nil {
		return storedDive{}, fmt.Errorf("encode FIT data: %w", err)
	}
	dive.FITJSON = encoded
	if document == nil {
		return dive, nil
	}
	if document.Version != fitDocumentVersion || len(document.Files) == 0 {
		return storedDive{}, errors.New("invalid or unsupported decoded FIT document")
	}
	dive.FITFiles, err = prepareFITFiles(document.Files)
	if err != nil {
		return storedDive{}, err
	}
	var records []fitMessage
	var alerts []diveAlert
	var summaries []fitMessage
	for _, file := range document.Files {
		for _, sequence := range file.Sequences {
			for _, message := range sequence.Messages {
				switch message.Number {
				case uint16(typedef.MesgNumRecord):
					if message.TimestampMS != nil {
						records = append(records, message)
					}
				case uint16(typedef.MesgNumEvent):
					if alert, ok := fitDiveAlert(message); ok {
						alert.Index = len(alerts)
						alerts = append(alerts, alert)
					}
				case uint16(typedef.MesgNumSession), uint16(typedef.MesgNumDiveSummary):
					summaries = append(summaries, message)
				}
			}
		}
	}
	// Session summaries describe the whole activity; dive_summary can also
	// describe an individual lap. Prefer the session for queryable columns.
	for _, kind := range []uint16{uint16(typedef.MesgNumSession), uint16(typedef.MesgNumDiveSummary)} {
		for _, summary := range summaries {
			if summary.Number == kind {
				fillFITSummary(&dive, summary)
			}
		}
	}
	if len(records) > 0 {
		sort.SliceStable(records, func(i, j int) bool { return *records[i].TimestampMS < *records[j].TimestampMS })
		samples := make([]diveSample, 0, len(records))
		keys := map[string]struct{}{"TIMESTAMP": {}}
		for _, record := range records {
			raw, err := json.Marshal(record)
			if err != nil {
				return storedDive{}, fmt.Errorf("encode FIT sample: %w", err)
			}
			sample := diveSample{Index: len(samples), TimestampMS: *record.TimestampMS,
				ElapsedMS: *record.TimestampMS - *records[0].TimestampMS, Values: make(map[string]*float64), RawJSON: raw}
			for _, field := range record.Fields {
				key := fitSampleKeys[field.Name]
				if key == "" {
					key = strings.ToUpper(field.Name)
					if key == "" || key == "UNKNOWN" {
						key = fmt.Sprintf("FIT_FIELD_%d", field.Number)
					}
				}
				keys[key] = struct{}{}
				sample.Values[key] = fitNumber(field.Value)
			}
			for _, field := range record.DeveloperFields {
				if field.DeveloperDataIndex != nil {
					keys[fmt.Sprintf("FIT_DEVELOPER_%d_%d", *field.DeveloperDataIndex, field.Number)] = struct{}{}
				}
			}
			samples = append(samples, sample)
		}
		dive.Samples = samples
		ordered := make([]string, 0, len(keys))
		for key := range keys {
			ordered = append(ordered, key)
		}
		sort.Strings(ordered)
		dive.RecordMetadata = make([]recordMetadata, len(ordered))
		for i, key := range ordered {
			dive.RecordMetadata[i] = recordMetadata{Position: i, Key: key}
		}
	}
	if len(alerts) > 0 {
		dive.Alerts = alerts
	}
	return dive, nil
}

func fitFieldNumber(message fitMessage, name string) *float64 {
	for _, field := range message.Fields {
		if field.Name == name {
			return fitNumber(field.Value)
		}
	}
	return nil
}

func fitDiveAlert(message fitMessage) (diveAlert, bool) {
	number := fitFieldNumber(message, "event")
	if number == nil {
		return diveAlert{}, false
	}
	event := typedef.Event(byte(*number))
	switch event {
	case typedef.EventDiveAlert, typedef.EventDiveGasSwitched, typedef.EventTankPressureReserve,
		typedef.EventTankPressureCritical, typedef.EventTankLost, typedef.EventTankBatteryLow,
		typedef.EventTankPodConnected, typedef.EventTankPodDisconnected:
	default:
		return diveAlert{}, false
	}
	name := event.String()
	if event == typedef.EventDiveAlert {
		if value := fitFieldNumber(message, "dive_alert"); value != nil {
			name = typedef.DiveAlert(byte(*value)).String()
		}
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return diveAlert{}, false
	}
	return diveAlert{TimestampMS: message.TimestampMS, Type: name, DataJSON: raw}, true
}

func fillFITSummary(dive *storedDive, message fitMessage) {
	for name, target := range map[string]**float64{
		"max_depth": &dive.MaxDepth, "avg_depth": &dive.AverageDepth,
		"avg_heart_rate": &dive.AverageHeartRate, "max_heart_rate": &dive.MaxHeartRate,
		"min_temperature": &dive.MinTemperature, "avg_temperature": &dive.AverageTemperature, "max_temperature": &dive.MaxTemperature,
		"total_timer_time": &dive.Duration,
	} {
		if *target == nil {
			*target = fitFieldNumber(message, name)
		}
	}
	if dive.DiveNumber == 0 {
		if number := fitFieldNumber(message, "dive_number"); number != nil && *number > 0 {
			dive.DiveNumber = int(*number)
		}
	}
	for name, target := range map[string]**float64{
		"start_position_lat": &dive.EntryLatitude, "start_position_long": &dive.EntryLongitude,
		"end_position_lat": &dive.ExitLatitude, "end_position_long": &dive.ExitLongitude,
	} {
		if *target == nil {
			if value := fitFieldNumber(message, name); value != nil {
				degrees := *value * 180 / math.Exp2(31)
				*target = &degrees
			}
		}
	}
}
