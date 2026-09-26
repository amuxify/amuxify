package remux

import (
	"context"

	"github.com/amuxify/amuxify/internal/probe"
	"github.com/amuxify/amuxify/internal/verify"
)

// streamHash and decodedHash are the hash calls sameStream uses. Tests in this
// package replace them to simulate a stream that differs after remux.
var streamHash = func(ctx context.Context, v *verify.Verifier, path string, index int) (string, error) {
	return v.StreamHash(ctx, path, index)
}
var decodedHash = func(ctx context.Context, v *verify.Verifier, path string, s probe.Stream) (string, error) {
	return v.DecodedHash(ctx, path, s)
}

// seamWired reports whether sameStream calls streamHash and decodedHash.
// The tests that replace the seams skip until the remuxer is wired to them.
var seamWired = true
