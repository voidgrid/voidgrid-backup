# Third-party licenses

This project's own code is MIT-licensed (see `LICENSE`). It uses
[Kopia](https://kopia.io) (`github.com/kopia/kopia`) as a Go library for the
storage engine — content-addressed, deduplicated, encrypted snapshots.
Kopia is licensed under the Apache License, Version 2.0:
<https://github.com/kopia/kopia/blob/master/LICENSE>. Apache-2.0 is
permissive and imposes no license requirement on projects that use it as a
dependency; this file exists to retain its copyright notice, as the license
asks of anyone distributing software that includes it (the built Docker
image does, since Kopia's code is compiled into both binaries).

Every other dependency is listed with its version in `go.mod`; run `go
list -m all` (inside `scripts/go.sh`) for the full tree, or a tool like
[`go-licenses`](https://github.com/google/go-licenses) to check them all at
once if that's ever needed.
