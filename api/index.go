// Package handler is the Vercel entry point. Everything else is in
// ../serverless, because Vercel builds this file outside the module's
// import path and so cannot reach internal/ from here.
package handler

import (
	"net/http"

	"github.com/adamchandra-2nd/api-house-moving-day/serverless"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	serverless.Handler(w, r)
}
