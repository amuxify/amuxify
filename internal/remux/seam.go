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

// beforePlace, when set, runs right before a verified temp file is placed,
// after the last check of the temp name and before the placement primitive
// uses that name again. Tests in this package set it to swap the temp name
// in that window; it is nil in production.
var beforePlace func(tmp, dest string)
