package protocol

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	original := &Request{
		ReqID: 42,
		Op:    OpLogin,
		Data:  json.RawMessage(`{"username":"alice","password":"secret"}`),
	}
	var buf bytes.Buffer
	var mu sync.Mutex
	if err := EncodeRequest(&buf, &mu, original); err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := NewFrameReader(&buf)
	if !s.Scan() {
		t.Fatalf("scan: %v", s.Err())
	}
	got, err := DecodeRequest(s.Bytes())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ReqID != original.ReqID || got.Op != original.Op {
		t.Errorf("mismatch: got %+v, want %+v", got, original)
	}
}

func TestResponseRoundTrip(t *testing.T) {
	original := &Response{
		ReqID:  7,
		Status: StatusOK,
		Data:   json.RawMessage(`{"user_id":100}`),
	}
	var buf bytes.Buffer
	var mu sync.Mutex
	if err := EncodeResponse(&buf, &mu, original); err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := NewFrameReader(&buf)
	if !s.Scan() {
		t.Fatalf("scan: %v", s.Err())
	}
	got, err := DecodeResponse(s.Bytes())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ReqID != original.ReqID || got.Status != original.Status {
		t.Errorf("mismatch: got %+v, want %+v", got, original)
	}
}

func TestDecodeMalformed(t *testing.T) {
	_, err := DecodeRequest([]byte("not json"))
	if err == nil {
		t.Error("expected error on malformed JSON")
	}
}
