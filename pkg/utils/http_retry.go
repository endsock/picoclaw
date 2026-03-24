package utils

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

const defaultMaxRetries = 3

var retryDelayUnit = time.Second

func shouldRetry(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests ||
		statusCode >= 500
}

func DoRequestWithRetry(client *http.Client, req *http.Request) (*http.Response, error) {
	return DoRequestWithRetryMaxRetries(client, req, defaultMaxRetries)
}

func DoRequestWithRetryMaxRetries(client *http.Client, req *http.Request, maxRetries int) (*http.Response, error) {
	if maxRetries <= 0 {
		maxRetries = defaultMaxRetries
	}

	var resp *http.Response
	var err error

	for i := range maxRetries {
		if i > 0 && resp != nil {
			resp.Body.Close()
		}

		attemptReq, reqErr := cloneRequestForRetry(req)
		if reqErr != nil {
			return nil, reqErr
		}

		resp, err = client.Do(attemptReq)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				break
			}
			if !shouldRetry(resp.StatusCode) {
				break
			}
		}

		if i < maxRetries-1 {
			if err = sleepWithCtx(req.Context(), retryDelayUnit*time.Duration(i+1)); err != nil {
				if resp != nil {
					resp.Body.Close()
				}
				return nil, fmt.Errorf("failed to sleep: %w", err)
			}
		}
	}
	return resp, err
}

func cloneRequestForRetry(req *http.Request) (*http.Request, error) {
	cloned := req.Clone(req.Context())
	if req.Body == nil || req.GetBody == nil {
		return cloned, nil
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("failed to clone request body: %w", err)
	}
	cloned.Body = body
	return cloned, nil
}

func sleepWithCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
