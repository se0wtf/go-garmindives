package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func testOpenAPI(t *testing.T) map[string]any {
	t.Helper()
	data, err := openAPIJSON()
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func openAPIObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T: %v", value, value)
	}
	return object
}

func resolveOpenAPIRef(t *testing.T, document map[string]any, value any) map[string]any {
	t.Helper()
	object := openAPIObject(t, value)
	if ref, ok := object["$ref"].(string); ok {
		if !strings.HasPrefix(ref, "#/") {
			t.Fatalf("OpenAPI reference must be embedded/local: %s", ref)
		}
		var current any = document
		for _, key := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			key = strings.ReplaceAll(strings.ReplaceAll(key, "~1", "/"), "~0", "~")
			current = openAPIObject(t, current)[key]
			if current == nil {
				t.Fatalf("unresolved OpenAPI reference: %s", ref)
			}
		}
		return openAPIObject(t, current)
	}
	return object
}

func TestOpenAPIDocumentsEveryAPIRoute(t *testing.T) {
	document := testOpenAPI(t)
	paths := openAPIObject(t, document["paths"])
	routes := map[string]bool{}
	// Read actual route registration, so new endpoints cannot silently be left
	// out of the frontend contract. Documentation/static UI routes are excluded.
	for _, filename := range []string{"api.go", "media_api.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "HandleFunc" && selector.Sel.Name != "Handle") {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			pattern, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			method, path, ok := strings.Cut(pattern, " ")
			if !ok || (!strings.HasPrefix(path, "/api/v1/") && !strings.HasPrefix(path, "/assets/")) {
				return true
			}
			routes[pattern] = true
			item := openAPIObject(t, paths[path])
			if item[strings.ToLower(method)] == nil {
				t.Errorf("route missing from OpenAPI: %s", pattern)
			}
			return true
		})
	}
	operationIDs := map[string]bool{}
	for path, value := range paths {
		item := openAPIObject(t, value)
		for _, method := range []string{"get", "head", "post", "patch", "delete"} {
			value, ok := item[method]
			if !ok {
				continue
			}
			registered := routes[strings.ToUpper(method)+" "+path]
			if method == "head" {
				registered = registered || routes["GET "+path]
			}
			if !registered {
				t.Errorf("documented route is not registered: %s %s", method, path)
			}
			op := openAPIObject(t, value)
			id, _ := op["operationId"].(string)
			if id == "" || operationIDs[id] {
				t.Errorf("missing/duplicate operationId for %s %s: %q", method, path, id)
			}
			operationIDs[id] = true
			if len(openAPIObject(t, op["responses"])) == 0 {
				t.Errorf("missing responses: %s %s", method, path)
			}
		}
		params := map[string]bool{}
		if values, ok := item["parameters"].([]any); ok {
			for _, value := range values {
				param := resolveOpenAPIRef(t, document, value)
				if param["in"] == "path" && param["required"] == true {
					params[param["name"].(string)] = true
				}
			}
		}
		for _, part := range strings.Split(path, "/") {
			if strings.HasPrefix(part, "{") && !params[strings.Trim(part, "{}")] {
				t.Errorf("missing required path parameter %s in %s", part, path)
			}
		}
	}
	var checkReferences func(any)
	checkReferences = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if _, ok := value["$ref"]; ok {
				resolveOpenAPIRef(t, document, value)
			}
			for _, child := range value {
				checkReferences(child)
			}
		case []any:
			for _, child := range value {
				checkReferences(child)
			}
		}
	}
	checkReferences(document)
}

func TestOpenAPISchemasMatchResponseFields(t *testing.T) {
	document := testOpenAPI(t)
	components := openAPIObject(t, document["components"])
	schemas := openAPIObject(t, components["schemas"])
	for name, value := range map[string]any{
		"DiveSummary": diveSummary{}, "DiveDetail": diveDetail{}, "DiveAlert": diveAlertAPIResponse{},
		"RecordMetadata": recordMetadata{}, "DiveSample": diveSampleAPIResponse{},
		"DiveList": diveListResponse{}, "SampleList": sampleListResponse{},
		"MediaItem": mediaItem{}, "MediaList": mediaListResponse{}, "MediaScanResult": mediaScanResult{},
		"FITDocument": fitDocument{}, "FITFile": fitFile{}, "FITSequence": fitSequence{},
		"FITMessage": fitMessage{}, "FITField": fitField{}, "ErrorResponse": errorResponse{},
		"DiveCoverPhoto": diveCoverPhoto{},
	} {
		t.Run(name, func(t *testing.T) {
			schema := openAPIObject(t, schemas[name])
			properties := openAPIObject(t, schema["properties"])
			required := map[string]bool{}
			for _, value := range schema["required"].([]any) {
				required[value.(string)] = true
			}
			typ := reflect.TypeOf(value)
			fields := map[string]bool{}
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
				if name == "-" || name == "" {
					continue
				}
				fields[name] = true
				if properties[name] == nil {
					t.Errorf("response field absent from schema: %s", name)
					continue
				}
				optional := strings.Contains(options, "omitempty")
				if required[name] == optional {
					t.Errorf("field %s required=%v, omitempty=%v", name, required[name], optional)
				}
				if field.Type == reflect.TypeOf(json.RawMessage{}) {
					continue // Raw retained JSON intentionally has a flexible schema.
				}
				fieldType := field.Type
				if fieldType.Kind() == reflect.Pointer {
					fieldType = fieldType.Elem()
				}
				var expected string
				switch fieldType.Kind() {
				case reflect.String:
					expected = "string"
				case reflect.Int, reflect.Int64, reflect.Uint8, reflect.Uint16:
					expected = "integer"
				case reflect.Float64:
					expected = "number"
				case reflect.Slice:
					expected = "array"
				case reflect.Struct:
					expected = "object"
				case reflect.Bool:
					expected = "boolean"
				}
				property := resolveOpenAPIRef(t, document, properties[name])
				if expected != "" && property["type"] != expected {
					t.Errorf("field %s schema type=%v, Go type=%s", name, property["type"], field.Type)
				}
			}
			for name := range properties {
				if !fields[name] {
					t.Errorf("schema field absent from response: %s", name)
				}
			}
		})
	}
}

func TestSwaggerUIAndOpenAPIDownloadsAreEmbedded(t *testing.T) {
	store, _ := openTestSQLStore(t)
	handler := newAPIHandler(store, testLogger(t))
	jsonResponse := mediaRequest(t, handler, "GET", "/api/v1/openapi.json", nil, "")
	yamlResponse := mediaRequest(t, handler, "GET", "/api/v1/openapi.yaml", nil, "")
	var jsonDoc, yamlDoc map[string]any
	if err := json.Unmarshal(jsonResponse.Body.Bytes(), &jsonDoc); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(yamlResponse.Body.Bytes(), &yamlDoc); err != nil {
		t.Fatal(err)
	}
	// Normalize YAML integer values before comparison with JSON numbers.
	data, err := json.Marshal(yamlDoc)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &yamlDoc); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(jsonDoc, yamlDoc) || jsonDoc["openapi"] != "3.0.3" {
		t.Fatal("JSON and YAML must expose the same OpenAPI contract")
	}
	if jsonResponse.Code != 200 || yamlResponse.Code != 200 || !strings.HasPrefix(jsonResponse.Header().Get("Content-Type"), "application/json") || jsonResponse.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("spec response: %d / %d, %v", jsonResponse.Code, yamlResponse.Code, jsonResponse.Header())
	}
	for path, expectedType := range map[string]string{
		"/swagger/": "text/html", "/swagger/index.html": "text/html",
		"/swagger/swagger-initializer.js": "text/javascript", "/swagger/swagger-ui-bundle.js": "text/javascript",
		"/swagger/swagger-ui.css": "text/css", "/swagger/LICENSE": "text/plain",
		"/swagger/swagger-ui-bundle.js.LICENSE.txt": "text/plain",
	} {
		response := mediaRequest(t, handler, "GET", path, nil, "")
		if response.Code != 200 || response.Body.Len() == 0 || !strings.HasPrefix(response.Header().Get("Content-Type"), expectedType) {
			t.Errorf("GET %s = %d %v", path, response.Code, response.Header())
		}
		request := httptest.NewRequest("GET", path, nil)
		request.Header.Set("If-None-Match", response.Header().Get("ETag"))
		cached := httptest.NewRecorder()
		handler.ServeHTTP(cached, request)
		if cached.Code != http.StatusNotModified {
			t.Errorf("conditional GET %s = %d", path, cached.Code)
		}
	}
	for _, path := range []string{"/swagger", "/docs", "/docs/"} {
		response := mediaRequest(t, handler, "GET", path, nil, "")
		if response.Code != http.StatusTemporaryRedirect || response.Header().Get("Location") != "/swagger/" {
			t.Errorf("docs redirect %s = %d, %v", path, response.Code, response.Header())
		}
	}
	for _, path := range []string{"/swagger/unknown", "/swagger/README.md", "/swagger/docs/openapi.yaml", "/docs/unknown"} {
		if response := mediaRequest(t, handler, "GET", path, nil, ""); response.Code != 404 {
			t.Errorf("unexpected documentation route %s = %d", path, response.Code)
		}
	}
	initializer := mediaRequest(t, handler, "GET", "/swagger/swagger-initializer.js", nil, "").Body.String()
	if !strings.Contains(initializer, "../api/v1/openapi.json") || !strings.Contains(initializer, "validatorUrl: null") {
		t.Fatal("Swagger UI must load the local spec without the online validator")
	}
}
