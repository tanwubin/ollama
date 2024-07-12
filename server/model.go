package server

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/convert"
	"github.com/ollama/ollama/llm"
	"github.com/ollama/ollama/template"
	"github.com/ollama/ollama/types/model"
)

var intermediateBlobs map[string]string = make(map[string]string)

type layerGGML struct {
	*Layer
	*llm.GGML
}

func parseFromModel(ctx context.Context, name model.Name, fn func(api.ProgressResponse)) (layers []*layerGGML, err error) {
	m, err := ParseNamedManifest(name)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := PullModel(ctx, name.String(), &registryOptions{}, fn); err != nil {
			return nil, err
		}

		m, err = ParseNamedManifest(name)
		if err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	}

	for _, layer := range m.Layers {
		layer, err := NewLayerFromLayer(layer.Digest, layer.MediaType, name.DisplayShortest())
		if err != nil {
			return nil, err
		}

		switch layer.MediaType {
		case "application/vnd.ollama.image.model",
			"application/vnd.ollama.image.projector",
			"application/vnd.ollama.image.adapter":
			blobpath, err := GetBlobsPath(layer.Digest)
			if err != nil {
				return nil, err
			}

			blob, err := os.Open(blobpath)
			if err != nil {
				return nil, err
			}
			defer blob.Close()

			ggml, _, err := llm.DecodeGGML(blob, 0)
			if err != nil {
				return nil, err
			}

			layers = append(layers, &layerGGML{layer, ggml})
		default:
			layers = append(layers, &layerGGML{layer, nil})
		}
	}

	return layers, nil
}

func extractFromZipFile(p string, file *os.File, fn func(api.ProgressResponse)) error {
	stat, err := file.Stat()
	if err != nil {
		return err
	}

	r, err := zip.NewReader(file, stat.Size())
	if err != nil {
		return err
	}

	fn(api.ProgressResponse{Status: "unpacking model metadata"})
	for _, f := range r.File {
		if !filepath.IsLocal(f.Name) {
			return fmt.Errorf("%w: %s", zip.ErrInsecurePath, f.Name)
		}

		n := filepath.Join(p, f.Name)
		if err := os.MkdirAll(filepath.Dir(n), 0o750); err != nil {
			return err
		}

		// TODO(mxyng): this should not write out all files to disk
		outfile, err := os.Create(n)
		if err != nil {
			return err
		}
		defer outfile.Close()

		infile, err := f.Open()
		if err != nil {
			return err
		}
		defer infile.Close()

		if _, err = io.Copy(outfile, infile); err != nil {
			return err
		}

		if err := outfile.Close(); err != nil {
			return err
		}

		if err := infile.Close(); err != nil {
			return err
		}
	}

	return nil
}

func parseFromZipFile(_ context.Context, file *os.File, digest string, fn func(api.ProgressResponse)) (layers []*layerGGML, err error) {
	tempDir, err := os.MkdirTemp(filepath.Dir(file.Name()), "")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)

	if err := extractFromZipFile(tempDir, file, fn); err != nil {
		return nil, err
	}

	mf, err := convert.GetModelFormat(tempDir)
	if err != nil {
		return nil, err
	}

	params, err := mf.GetParams(tempDir)
	if err != nil {
		return nil, err
	}

	mArch, err := mf.GetModelArch("", tempDir, params)
	if err != nil {
		return nil, err
	}

	fn(api.ProgressResponse{Status: "processing tensors"})
	if err := mArch.GetTensors(); err != nil {
		return nil, err
	}

	if err := mArch.LoadVocab(); err != nil {
		return nil, err
	}

	fn(api.ProgressResponse{Status: "converting model"})

	// TODO(mxyng): this should write directly into a layer
	// e.g. NewLayer(arch.Reader(), "application/vnd.ollama.image.model")
	temp, err := os.CreateTemp(tempDir, "fp16")
	if err != nil {
		return nil, err
	}
	defer temp.Close()
	defer os.Remove(temp.Name())

	if err = mArch.WriteGGUF(temp); err != nil {
		return nil, err
	}

	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	layer, err := NewLayer(temp, "application/vnd.ollama.image.model")
	if err != nil {
		return nil, err
	}

	bin, err := layer.Open()
	if err != nil {
		return nil, err
	}
	defer bin.Close()

	ggml, _, err := llm.DecodeGGML(bin, 0)
	if err != nil {
		return nil, err
	}

	layers = append(layers, &layerGGML{layer, ggml})

	intermediateBlobs[digest] = layer.Digest
	return detectChatTemplate(layers)
}

func parseFromFile(ctx context.Context, file *os.File, digest string, fn func(api.ProgressResponse)) (layers []*layerGGML, err error) {
	sr := io.NewSectionReader(file, 0, 512)
	contentType, err := detectContentType(sr)
	if err != nil {
		return nil, err
	}

	switch contentType {
	case "gguf", "ggla":
		// noop
	case "application/zip":
		return parseFromZipFile(ctx, file, digest, fn)
	default:
		return nil, fmt.Errorf("unsupported content type: %s", contentType)
	}

	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}

	var offset int64
	for offset < stat.Size() {
		ggml, n, err := llm.DecodeGGML(file, -1)
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}

		mediatype := "application/vnd.ollama.image.model"
		if ggml.Name() == "ggla" {
			mediatype = "application/vnd.ollama.image.adapter"
		} else if ggml.KV().Architecture() == "clip" {
			mediatype = "application/vnd.ollama.image.projector"
		}

		var reader io.Reader = io.NewSectionReader(file, offset, n)
		if !sort.IsSorted(ggml.Tensors()) {
			// create a new Tensors containing Tensors that have a writeTo
			var tensors llm.Tensors

			for _, tensor := range ggml.Tensors() {
				shape := make([]uint64, len(tensor.Shape))
				for i := range len(tensor.Shape) {
					shape[i] = tensor.Shape[len(tensor.Shape)-i-1]
				}

				tensors = append(tensors, &llm.Tensor{
					Name:  tensor.Name,
					Kind:  tensor.Kind,
					Shape: shape,

					WriterTo: &llm.TensorWriter{
						Reader: io.NewSectionReader(file, int64(tensor.Offset), int64(tensor.Size())),
					},
				})
			}

			reader = &llm.GGUFWriter{
				KV: ggml.KV(),
				// Update .Tensors
				Tensors: tensors,
			}
		}

		layer, err := NewLayer(reader, mediatype)
		if err != nil {
			return nil, err
		}

		layers = append(layers, &layerGGML{layer, ggml})
		offset = n
	}

	return detectChatTemplate(layers)
}

func detectChatTemplate(layers []*layerGGML) ([]*layerGGML, error) {
	for _, layer := range layers {
		if s := layer.GGML.KV().ChatTemplate(); s != "" {
			if t, err := template.Named(s); err != nil {
				slog.Debug("template detection", "error", err)
			} else {
				tmpl, err := NewLayer(t.Reader(), "application/vnd.ollama.image.template")
				if err != nil {
					return nil, err
				}

				tmpl.status = fmt.Sprintf("using autodetected template %s", t.Name)
				layers = append(layers, &layerGGML{tmpl, nil})
			}
		}
	}

	return layers, nil
}

func detectContentType(r io.Reader) (string, error) {
	var b bytes.Buffer
	if _, err := io.Copy(&b, r); err != nil {
		return "", err
	}

	if contentType := llm.DetectGGMLType(b.Bytes()); contentType != "" {
		return contentType, nil
	}

	if contentType := http.DetectContentType(b.Bytes()); contentType != "application/octet-stream" {
		return contentType, nil
	}

	return "unknown", nil
}
