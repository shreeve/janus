# Files precompressed-sidecar measurement

This point-in-time measurement records the cost of Janus's transparent
sidecar selection on the registered-files path. It is descriptive evidence,
not an acceptance threshold.

## Method

```shell
go test -run '^$' -bench '^BenchmarkServeFiles$' -benchmem -count=5
```

Host: Apple M5, Darwin arm64, Go 1.26.5. The benchmark uses temporary local
files, `httptest`, the default `br zstd gzip` preference, and a request for
`Accept-Encoding: br`. Raw output is
[`20260805-022544-bench-raw-files-precompressed.txt`](20260805-022544-bench-raw-files-precompressed.txt).

## Result

| Path | Median | Bytes/op | Allocations/op |
| --- | ---: | ---: | ---: |
| First-root identity hit | 19.1µs | 7,199 | 42 |
| First-root Brotli sidecar hit | 28.3µs | 8,176 | 58 |
| Second-root identity after missing sidecar | 28.7µs | 7,975 | 58 |

The sidecar hit adds negotiation and one descriptor-relative sidecar
open/stat to the canonical-file lookup. The missing-sidecar path pays the
same bounded lookup cost before serving the already-open canonical file.
No compression occurs on this path; publishers create the bytes ahead of
time.
