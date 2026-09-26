# Security policy

amuxify processes untrusted files by design. A bug that lets a crafted file
make amuxify or the tools it runs do something other than read that file, or
that lets an output be placed without passing verification, is a security bug.

## Reporting

Email security@amuxify.com with a description and, if possible, a fixture that
reproduces it. Do not open a public issue for exploitable bugs. You will get an
acknowledgement within 72 hours and a fix or mitigation plan within 14 days.

## Scope

- Any way to bypass a `BLOCK` verdict.
- Any way to make ffmpeg or ffprobe open a non-file protocol.
- Any way for a remux output to differ from the source streams while reporting
  `HASH_OK`.
- Any write outside the destination directory, any overwrite of an existing
  path, any symlink traversal.
- Any removal of extended attributes outside `user.*` and `com.apple.*`.

Out of scope: vulnerabilities in ffmpeg, MKVToolNix or ClamAV themselves
(report those upstream; amuxify's job is to limit their exposure), and
findings that require the attacker to already control the profile file or the
environment.

## Supported versions

The latest minor release. Security fixes are backported to the previous minor
release for 90 days after a new minor is published.
