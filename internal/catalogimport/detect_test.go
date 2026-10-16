package catalogimport

import (
	"errors"
	"testing"
)

func TestDetect(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want string
	}{
		"openapi json":   {`{"openapi":"3.1.0","paths":{}}`, OpenAPI3},
		"openapi yaml":   {"openapi: 3.0.3\ninfo:\n  title: x\n", OpenAPI3},
		"swagger json":   {`{"swagger":"2.0"}`, Swagger2},
		"swagger yaml":   {"swagger: '2.0'\n", Swagger2},
		"postman schema": {`{"info":{"schema":"https://schema.getpostman.com/json/collection/v2.1.0/collection.json"},"item":[]}`, Postman},
		"postman id":     {`{"info":{"_postman_id":"abc"},"item":[]}`, Postman},
		"discovery":      {`{"kind":"discovery#restDescription","resources":{}}`, Discovery},
		"discovery ver":  {`{"discoveryVersion":"v1"}`, Discovery},
		"introspection":  {`{"data":{"__schema":{"types":[]}}}`, GraphQL},
		"introspection2": {`{"__schema":{"types":[]}}`, GraphQL},
		"sdl":            {"type Query {\n  me: User\n}\ntype User { id: ID }\n", GraphQL},
	} {
		got, err := Detect([]byte(tc.in))
		if err != nil || got != tc.want {
			t.Errorf("%s: %q, %v, want %q", name, got, err, tc.want)
		}
	}
	for name, tc := range map[string]struct {
		in  string
		err error
	}{
		"empty":        {"  ", ErrInvalid},
		"prose":        {"hello there", ErrInvalid},
		"unknown json": {`{"a":1}`, ErrInvalid},
		"array":        {`[1,2]`, ErrInvalid},
		"openapi 2.x":  {`{"openapi":"2.5"}`, ErrUnsupported},
		"swagger 1.2":  {`{"swagger":"1.2"}`, ErrUnsupported},
		"asyncapi":     {`{"asyncapi":"2.6.0"}`, ErrUnsupported},
		"wsdl":         {`<?xml version="1.0"?><definitions/>`, ErrUnsupported},
		"raml":         {"#%RAML 1.0\ntitle: x\n", ErrUnsupported},
		"blueprint":    {"FORMAT: 1A\n# API\n", ErrUnsupported},
	} {
		if _, err := Detect([]byte(tc.in)); !errors.Is(err, tc.err) {
			t.Errorf("%s: %v, want %v", name, err, tc.err)
		}
	}
	if !Valid(Postman) || Valid("raml") {
		t.Error("Valid")
	}
}
