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

// beforeIdentity, when set, runs in the in-place branches after the last
// check of the temp name and right before takeIdentity opens that name to
// give the output its source's mode, ownership and time. Tests in this
// package set it to swap the temp name in that window; it is nil in
// production.
var beforeIdentity func(tmp, dest string)

// beforePlace, when set, runs right before a verified temp file is placed,
// after the last check of the temp name and, in place, after the identity
// copy has closed its descriptor, so that the placement primitive is the
// next thing to use the name. Tests in this package set it to swap the temp
// name in that window; it is nil in production.
var beforePlace func(tmp, dest string)
