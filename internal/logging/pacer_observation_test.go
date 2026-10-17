package logging

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestPacerHeaderObservationClocks(t *testing.T) {
	ctx := WithFreshResponseHeadersHolder(context.Background())
	start := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	setResponseHeadersAt(ctx, http.Header{"X-Codex-Primary-Reset-After-Seconds": {"60"}, "X-Codex-Secondary-Reset-After-Seconds": {"120"}}, start)
	mergeResponseHeadersAt(ctx, http.Header{"X-Codex-Primary-Reset-After-Seconds": {"10"}}, start.Add(time.Minute))
	headers, at := GetResponseHeadersSnapshot(ctx)
	if headers.Get("X-Codex-Primary-Reset-After-Seconds") != "10" || !at["X-Codex-Primary-Reset-After-Seconds"].Equal(start.Add(time.Minute)) || !at["X-Codex-Secondary-Reset-After-Seconds"].Equal(start) {
		t.Fatalf("field-specific clocks lost: %+v", at)
	}
	at["X-Codex-Primary-Reset-After-Seconds"] = start.Add(time.Hour)
	_, again := GetResponseHeadersSnapshot(ctx)
	if !again["X-Codex-Primary-Reset-After-Seconds"].Equal(start.Add(time.Minute)) {
		t.Fatal("mutable snapshot")
	}
	_, fresh := GetResponseHeadersSnapshot(WithFreshResponseHeadersHolder(ctx))
	if len(fresh) != 0 {
		t.Fatal("retry inherited previous clocks")
	}
}
