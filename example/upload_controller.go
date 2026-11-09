package main

import (
	"bufio"

	"github.com/axkit/errors"
	"github.com/axkit/vatel"
	"github.com/axkit/vatel/auth"
	"github.com/valyala/fasthttp"
)

// errFileUnreadable is returned when the uploaded file cannot be read, e.g. a
// line is longer than the scanner buffer.
var errFileUnreadable = errors.Template("uploaded file cannot be read").StatusCode(fasthttp.StatusUnprocessableEntity)

// ImportResult is the response body for the import endpoint.
type ImportResult struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	Rows     int    `json:"rows"`
}

// ImportController handles multipart CSV file uploads.
// The factory sets Upload.Key so BindFile knows which form field to read.
//
// POST /import
// Content-Type: multipart/form-data
// Form field: "file"
type ImportController struct {
	vatel.Upload
	vatel.Output[ImportResult]
}

func (c *ImportController) Handle(ctx vatel.Context) error {
	// A missing "file" field is a 400 vatel.ErrInvalidRequestBody.
	if err := c.BindFile(ctx); err != nil {
		return err
	}

	rows, err := c.countRows()
	if err != nil {
		return errFileUnreadable.Wrap(err)
	}

	c.Out = ImportResult{
		Filename: c.Header.Filename,
		Size:     c.Header.Size,
		Rows:     rows,
	}
	return nil
}

// countRows streams the uploaded file line by line without loading it into memory.
// This pattern scales to large files — replace with real CSV parsing as needed.
func (c *ImportController) countRows() (int, error) {
	f, err := c.Open()
	if err != nil {
		return 0, err
	}
	defer f.Close()

	rows := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		rows++
	}
	return rows, scanner.Err()
}

// ── Endpoint Registration ─────────────────────────────────────────────────────

func ImportEndpoints(cfg *auth.Config) vatel.Endpoints {
	return vatel.Endpoints{
		vatel.POST("/import",
			func() vatel.Handler {
				return &ImportController{Upload: vatel.Upload{Key: "file"}}
			},
			cfg.RequirePerms(map[string]uint{"import": Permissions["import"]}),
			vatel.WithSuccessStatusCode(fasthttp.StatusCreated),
		),
	}
}
