package main

import "net/http"

// httpClient is shared across both API clients. Per-request timeouts are
// applied via context, mirroring the TypeScript AbortSignal.timeout usage.
var httpClient = &http.Client{}
