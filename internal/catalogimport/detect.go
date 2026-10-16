// Package catalogimport recognises the API description a person wants to turn
// into a connector, before it is sent anywhere.
package catalogimport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Formats the gateway turns into connectors.
const (
	OpenAPI3  = "openapi3"
	Swagger2  = "swagger2"
	Postman   = "postman"
	GraphQL   = "graphql"
	Discovery = "discovery"
)

// Formats lists what --format accepts besides auto.
var Formats = []string{OpenAPI3, Swagger2, Postman, GraphQL, Discovery}

// MaxSize bounds a description read from a file.
const MaxSize = 8 << 20

// ErrInvalid marks a document that cannot be read as an API description at
// all; ErrUnsupported one that is a description in a format that is not
// supported.
var (
	ErrInvalid     = errors.New("not an API description")
	ErrUnsupported = errors.New("an API description in an unsupported format")
)

var (
	graphQLSDL   = regexp.MustCompile(`(?m)^\s*(extend\s+)?(type|schema|input|enum|interface)\s+\w*\s*[{(@]`)
	wsdlOrRAML   = regexp.MustCompile(`(?i)^\s*(<\?xml|<definitions|<wsdl:|#%RAML)`)
	apiBlueprint = regexp.MustCompile(`(?im)^format:\s*1a`)
)

// Detect names the format of a description, or says why it has none.
func Detect(raw []byte) (string, error) {
	text := bytes.TrimSpace(raw)
	if len(text) == 0 {
		return "", fmt.Errorf("%w: the file is empty", ErrInvalid)
	}
	if wsdlOrRAML.Match(text) || apiBlueprint.Match(text) {
		return "", fmt.Errorf("%w: WSDL, RAML and API Blueprint are not imported; convert it to OpenAPI first", ErrUnsupported)
	}
	var doc map[string]any
	if json.Unmarshal(text, &doc) != nil {
		var y any
		if yaml.Unmarshal(text, &y) == nil {
			if m, ok := y.(map[string]any); ok {
				doc = m
			}
		}
	}
	if doc != nil {
		if f, err := detectDocument(doc); f != "" || err != nil {
			return f, err
		}
	}
	if graphQLSDL.Match(text) && !bytes.HasPrefix(text, []byte("{")) {
		return GraphQL, nil
	}
	if doc == nil {
		return "", fmt.Errorf("%w: it is neither JSON, YAML nor GraphQL", ErrInvalid)
	}
	return "", fmt.Errorf("%w: no openapi, swagger, Postman, discovery or GraphQL marker", ErrInvalid)
}

func detectDocument(doc map[string]any) (string, error) {
	if v, ok := doc["openapi"].(string); ok {
		if strings.HasPrefix(v, "3.") {
			return OpenAPI3, nil
		}
		return "", fmt.Errorf("%w: OpenAPI %s", ErrUnsupported, v)
	}
	if v, ok := doc["swagger"]; ok {
		if fmt.Sprint(v) == "2.0" || fmt.Sprint(v) == "2" {
			return Swagger2, nil
		}
		return "", fmt.Errorf("%w: Swagger %v", ErrUnsupported, v)
	}
	if info, ok := doc["info"].(map[string]any); ok {
		if schema, _ := info["schema"].(string); strings.Contains(schema, "getpostman.com/json/collection") {
			return Postman, nil
		}
		if _, ok := info["_postman_id"]; ok {
			if _, ok := doc["item"]; ok {
				return Postman, nil
			}
		}
	}
	if k, _ := doc["kind"].(string); k == "discovery#restDescription" {
		return Discovery, nil
	}
	if _, ok := doc["discoveryVersion"]; ok {
		return Discovery, nil
	}
	if _, ok := doc["__schema"]; ok {
		return GraphQL, nil
	}
	if data, ok := doc["data"].(map[string]any); ok {
		if _, ok := data["__schema"]; ok {
			return GraphQL, nil
		}
	}
	if _, ok := doc["asyncapi"]; ok {
		return "", fmt.Errorf("%w: AsyncAPI describes events, not calls", ErrUnsupported)
	}
	return "", nil
}

// Valid reports whether f is a format the gateway imports.
func Valid(f string) bool {
	for _, x := range Formats {
		if f == x {
			return true
		}
	}
	return false
}
