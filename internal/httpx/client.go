package httpx

import "net/http"

// Client is shared across both API clients. Per-request timeouts are
// applied via context, mirroring the TypeScript AbortSignal.timeout usage.
var Client = &http.Client{}
