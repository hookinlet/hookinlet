// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"reflect"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/internal/api"
)

// The document can grow a query parameter, and the generated code will bind it;
// nothing but this test notices if the handler then ignores it. Each field of each
// generated parameter struct is set on its own, and the form the handler works
// from has to change.
func TestEveryGeneratedQueryParameterIsRead(t *testing.T) {
	check := func(name string, zero any, set func(field int, v reflect.Value) any, convert func(any) any) {
		rt := reflect.TypeOf(zero)
		base := convert(zero)
		for i := range rt.NumField() {
			p := reflect.New(rt).Elem()
			val := valueFor(t, p.Field(i).Type())
			p.Field(i).Set(val)
			if reflect.DeepEqual(convert(p.Interface()), base) {
				t.Errorf("%s: the parameter %s is bound by the generated code and read by nothing", name, rt.Field(i).Name)
			}
		}
	}
	check("listMessages", api.ListMessagesParams{}, nil, func(v any) any { return messagesParams(v.(api.ListMessagesParams)) })
	check("headListMessages", api.HeadListMessagesParams{}, nil, func(v any) any { return headMessagesParams(v.(api.HeadListMessagesParams)) })
	check("streamMessages", api.StreamMessagesParams{}, nil, func(v any) any { return streamParams(v.(api.StreamMessagesParams)) })
}

// valueFor makes a pointer to a plausible non-zero value of the type a generated
// optional parameter has.
func valueFor(t *testing.T, ty reflect.Type) reflect.Value {
	t.Helper()
	if ty.Kind() != reflect.Pointer {
		t.Fatalf("a parameter of type %s is not optional", ty)
	}
	p := reflect.New(ty.Elem())
	switch e := ty.Elem(); {
	case e == reflect.TypeOf(time.Time{}):
		p.Elem().Set(reflect.ValueOf(time.Unix(1_700_000_000, 0)))
	case e.Kind() == reflect.Int:
		p.Elem().SetInt(7)
	case e.Kind() == reflect.Bool:
		p.Elem().SetBool(true)
	case e.Kind() == reflect.String:
		p.Elem().SetString("summary") // the one enum value that changes anything
	case e.Kind() == reflect.Slice && e.Elem().Kind() == reflect.String:
		p.Elem().Set(reflect.ValueOf([]string{"a"}))
	default:
		t.Fatalf("no sample value for %s", ty)
	}
	return p
}
