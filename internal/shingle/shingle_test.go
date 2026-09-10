package shingle

import (
	"fmt"
	"strings"
	"testing"
)

func words(n int, seed string) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "%s%d ", seed, i)
	}
	return sb.String()
}

func TestAppendedTurnIsHighOverlap(t *testing.T) {
	base := words(400, "w")
	grown := base + words(60, "new")
	a, b := Of(base), Of(grown)
	if ov := Overlap(a, b); ov < 0.95 {
		t.Fatalf("base should be almost fully inside grown, got %.2f", ov)
	}
	if ov := Overlap(b, a); ov > 0.95 || ov < 0.75 {
		t.Fatalf("grown vs base should be ~85%%, got %.2f", ov)
	}
}

func TestPrefixInsertionDoesNotShift(t *testing.T) {
	body := words(400, "w")
	a := Of(body)
	b := Of("INSERTED PREFIX TOKENS HERE " + body)
	if ov := Overlap(a, b); ov < 0.9 {
		t.Fatalf("content-defined sampling should survive prefix insertion, got %.2f", ov)
	}
}

func TestDisjoint(t *testing.T) {
	if ov := Overlap(Of(words(300, "a")), Of(words(300, "b"))); ov > 0.02 {
		t.Fatalf("disjoint texts overlap %.2f", ov)
	}
}

func TestShortText(t *testing.T) {
	if len(Of("hi there")) != 1 {
		t.Fatal("short text should yield one fingerprint")
	}
	if len(Of("")) != 0 {
		t.Fatal("empty text should yield none")
	}
}
