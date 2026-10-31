package foreman

import (
	"fmt"
	"net/url"
)

// srtProxyBridge is where srt's Linux sandbox listens for the egress proxy inside the run's network
// namespace. The sandbox cannot reach the host's loopback, so the host's port means nothing there:
// srt bridges its configured proxy to this fixed port instead.
const srtProxyBridge = "localhost:3128"

// modelBaseURL is the OpenRouter base URL as the model sees it. On Linux the model runs in its own
// network namespace and reaches the egress proxy through srt's bridge; elsewhere the sandbox shares
// the host's loopback and the proxy's own address works.
func modelBaseURL(goos, hostURL string) (string, error) {
	if goos != "linux" {
		return hostURL, nil
	}

	u, err := url.Parse(hostURL)
	if err != nil {
		return "", fmt.Errorf("parse the egress proxy URL: %w", err)
	}

	u.Host = srtProxyBridge

	return u.String(), nil
}
