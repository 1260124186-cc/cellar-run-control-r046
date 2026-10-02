package httpapi

import (
	"net/http"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
)

const maxFilenameLength = 255

type startImportRequest struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

type importResponse struct {
	domain.ImportSession
	Reused bool `json:"reused"`
}

func registerImportRoutes(mux *http.ServeMux, app *service.Service) {
	mux.HandleFunc("POST /v1/imports", func(response http.ResponseWriter, request *http.Request) {
		body, err := decodeJSON[startImportRequest](response, request)
		if !handleDecode(response, err) {
			return
		}
		if len([]rune(body.Filename)) > maxFilenameLength {
			writeDomainError(response, domain.FieldError(domain.CodeInvalidInput,
				"filename", "must not exceed 255 characters"))
			return
		}
		if body.Content == "" {
			writeDomainError(response, domain.FieldError(domain.CodeInvalidInput,
				"content", "is required"))
			return
		}
		session, reused, err := app.StartImport(body.Filename, []byte(body.Content))
		if err != nil {
			writeDomainError(response, err)
			return
		}
		status := http.StatusCreated
		if reused {
			status = http.StatusOK
		}
		writeJSON(response, status, importResponse{ImportSession: session, Reused: reused})
	})

	mux.HandleFunc("GET /v1/imports", func(response http.ResponseWriter, request *http.Request) {
		sessions, err := app.ListImports()
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"imports": sessions})
	})

	mux.HandleFunc("GET /v1/imports/{id}", func(response http.ResponseWriter, request *http.Request) {
		session, err := app.GetImport(request.PathValue("id"))
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, session)
	})

	mux.HandleFunc("POST /v1/imports/{id}/confirm", func(response http.ResponseWriter, request *http.Request) {
		session, err := app.ConfirmImport(request.PathValue("id"))
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, session)
	})
}
