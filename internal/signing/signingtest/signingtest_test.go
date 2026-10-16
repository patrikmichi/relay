package signingtest

import (
	"errors"
	"net/http"
	"testing"

	"github.com/patrikmichi/zuano/internal/signing"
)

func TestSignedBodyVerifies(t *testing.T) {
	s := New("k")
	h := http.Header{}
	s.Sign(h, []byte(`{"a":1}`))
	if err := signing.VerifyContent([]signing.Key{s.Key}, h, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
}

func TestSignPanicsOnABodyThatIsNotJSON(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic")
		}
	}()
	New("k").Sign(http.Header{}, []byte("nope"))
}

func TestMustPanicsOnError(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic")
		}
	}()
	must(0, errors.New("x"))
}

func TestMust2PanicsOnError(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic")
		}
	}()
	must2(0, 0, errors.New("x"))
}
