package httpapi

import (
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
)

const maxImportRequest = 11 << 20

func registerImportRoutes(mux *http.ServeMux, app *service.Service) {
	mux.HandleFunc("POST /v1/imports", func(response http.ResponseWriter, request *http.Request) {
		content, filename, err := readImportBody(request)
		if err != nil {
			writeDomainError(response, err)
			return
		}
		summary, created, err := app.PreviewImport(content, filename)
		if err != nil {
			writeDomainError(response, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(response, status, summary)
	})

	mux.HandleFunc("GET /v1/imports", func(response http.ResponseWriter, _ *http.Request) {
		summaries, err := app.ListImports()
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"imports": summaries})
	})

	mux.HandleFunc("GET /v1/imports/{id}", func(response http.ResponseWriter, request *http.Request) {
		summary, err := app.GetImport(request.PathValue("id"))
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, summary)
	})

	mux.HandleFunc("POST /v1/imports/{id}/confirm", func(response http.ResponseWriter, request *http.Request) {
		summary, err := app.ConfirmImport(request.PathValue("id"))
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, summary)
	})
}

// readImportBody accepts either a raw text/csv body or a multipart form
// carrying a "file" field. The response writer is used only to enforce the
// request size limit.
func readImportBody(request *http.Request) ([]byte, string, error) {
	contentType := request.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := request.ParseMultipartForm(maxImportRequest); err != nil {
			return nil, "", domain.FieldError(domain.CodeImportInvalid,
				"file", "the multipart form could not be parsed")
		}
		file, header, err := request.FormFile("file")
		if err != nil {
			return nil, "", domain.FieldError(domain.CodeImportInvalid,
				"file", "a multipart file field named file is required")
		}
		defer file.Close()
		content, err := io.ReadAll(io.LimitReader(file, maxImportRequest+1))
		if err != nil {
			return nil, "", domain.NewError(domain.CodeStorage, err.Error())
		}
		return content, header.Filename, nil
	}

	if request.Body == nil {
		return nil, "", domain.FieldError(domain.CodeImportInvalid,
			"body", "a CSV request body is required")
	}
	body := http.MaxBytesReader(nil, request.Body, maxImportRequest)
	content, err := io.ReadAll(body)
	if err != nil {
		return nil, "", domain.FieldError(domain.CodeImportInvalid,
			"body", "the CSV file must not exceed 10 MiB")
	}
	return content, filenameFromHeader(request), nil
}

func filenameFromHeader(request *http.Request) string {
	if disposition := request.Header.Get("Content-Disposition"); disposition != "" {
		if _, params, err := mime.ParseMediaType(disposition); err == nil {
			if name := params["filename"]; name != "" {
				return name
			}
		}
	}
	return "season-upload.csv"
}
