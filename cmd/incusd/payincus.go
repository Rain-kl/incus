package main

import (
	"net/http"
	"os"

	"github.com/lxc/incus/v7/internal/server/auth"
	"github.com/lxc/incus/v7/internal/server/payincus"
	nodesecurity "github.com/lxc/incus/v7/internal/server/payincus/security"
	"github.com/lxc/incus/v7/internal/server/response"
	"github.com/lxc/incus/v7/shared/logger"
)

// PayIncus: one authenticated subtree keeps extensions separate from upstream endpoints; see FEATURES.md.
func registerPayIncus(d *Daemon, router *http.ServeMux) {
	if os.Getenv("INCUS_PAYINCUS") == "1" {
		startPayIncusAudit(d)
	}
	service := payincus.New(d.shutdownCtx, os.Getenv("INCUS_PAYINCUS") == "1")
	handler := func(_ *Daemon, r *http.Request) response.Response {
		if r.URL.Query().Has("target") {
			return response.BadRequest(nil)
		}

		return response.ManualResponse(func(w http.ResponseWriter) error { service.ServeHTTP(w, r); return nil })
	}

	action := APIEndpointAction{Handler: handler, AccessHandler: allowPermission(auth.ObjectTypeServer, auth.EntitlementCanEdit)}
	d.createCmd(router, "1.0", APIEndpoint{Path: "payincus", Get: action, Put: action, Post: action})
	// PayIncus: ServeMux requires an explicit trailing-slash subtree for extension child routes.
	d.createCmd(router, "1.0", APIEndpoint{Path: "payincus/", Get: action, Put: action, Post: action})
	go func() {
		defer func() {
			if recover() != nil {
				logger.Error("PayIncus worker stopped after panic; inspect node status")
			}
		}()
		select {
		case <-d.shutdownCtx.Done():
			return
		case <-d.setupChan:
		}

		if os.Getenv("INCUS_PAYINCUS") == "1" {
			go nodesecurity.Default().Run(d.shutdownCtx)
		}
		service.Run()
	}()
}
