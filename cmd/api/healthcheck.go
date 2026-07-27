package main

import "net/http"

// healthcheckHandler reports that the API is up, plus some basic build info.
//
//	GET /v1/healthcheck
//
// This is the endpoint a load balancer or orchestrator polls to decide whether
// to route traffic here. It's deliberately trivial and does not touch the
// database — a healthcheck that can fail for reasons unrelated to the process
// being alive causes more outages than it prevents.
func (app *application) healthcheckHandler(w http.ResponseWriter, r *http.Request) {
	env := envelope{
		"status": "available",
		"system_info": map[string]string{
			"environment": app.config.env,
			"version":     version,
		},
	}

	err := app.writeJSON(w, http.StatusOK, env, nil)
	if err != nil {
		app.serverErrorResponse(w, r, err)
	}
}
