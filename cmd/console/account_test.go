package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// An account is the caller's own data: the screen shows it field by field, in
// a fixed order so two identical payments never read differently.
func TestReadAccount(t *testing.T) {
	got := readAccount(`{"pinfl":"52009037040022","order_id":"bdfe1e5e-f3ed-4ec5-80f1-5317953c77b2","full_name":"ABROR GAFUROV"}`)

	assert.Equal(t, []accountField{
		{Name: "full_name", Value: "ABROR GAFUROV"},
		{Name: "order_id", Value: "bdfe1e5e-f3ed-4ec5-80f1-5317953c77b2"},
		{Name: "pinfl", Value: "52009037040022"},
	}, got)
}

// A value that is not a string is printed as it arrived rather than dropped.
func TestReadAccountKeepsEveryValue(t *testing.T) {
	got := readAccount(`{"amount":500000,"paid":true,"note":null,"tags":["a","b"]}`)

	assert.Equal(t, []accountField{
		{Name: "amount", Value: "500000"},
		{Name: "note", Value: "—"},
		{Name: "paid", Value: "true"},
		{Name: "tags", Value: `["a","b"]`},
	}, got)
}

// An account the screen cannot read falls back to printing it whole, which is
// what the template does when there are no fields.
func TestReadAccountRejectsJunk(t *testing.T) {
	assert.Nil(t, readAccount("not json"))
	assert.Nil(t, readAccount(""))
	assert.Nil(t, readAccount(`["not","an","object"]`))
	assert.Empty(t, readAccount(`{}`))
}
