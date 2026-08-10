// Package embed is a from-scratch transformer encoder: safetensors loading, a
// WordPiece tokenizer, and a BERT forward pass in pure Go.
//
// An encoder is a far smaller thing to write than a decoder — no KV cache, no
// sampling, no incremental generation, fixed shapes — which is exactly why
// this is the layer worth owning rather than borrowing. It makes the retrieval
// stack fully self-contained: no embedding server, no API key, no network.
package embed

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
)

// Tensor is a loaded weight, always converted to float32 regardless of how it
// was stored.
type Tensor struct {
	Name  string
	Shape []int
	Data  []float32
}

func (t *Tensor) Rows() int {
	if len(t.Shape) == 0 {
		return 0
	}
	return t.Shape[0]
}

func (t *Tensor) Cols() int {
	if len(t.Shape) < 2 {
		return len(t.Data)
	}
	n := 1
	for _, d := range t.Shape[1:] {
		n *= d
	}
	return n
}

// Weights is a loaded safetensors file.
type Weights struct {
	byName map[string]*Tensor
	// skipped records tensors present in the file but not decodable, keyed by
	// name and holding the dtype. See LoadSafetensors for why they are not an
	// error on their own.
	skipped map[string]string
}

// SkippedDType reports whether a tensor was present but left undecoded, and
// what dtype it had. Used to turn "missing tensor" into a precise message.
func (w *Weights) SkippedDType(name string) (string, bool) {
	d, ok := w.skipped[name]
	return d, ok
}

func (w *Weights) Get(name string) (*Tensor, bool) {
	t, ok := w.byName[name]
	return t, ok
}

// Names returns every tensor name, sorted. Used by the diagnostics command so
// a layout mismatch is inspectable rather than a silent failure.
func (w *Weights) Names() []string {
	out := make([]string, 0, len(w.byName))
	for n := range w.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (w *Weights) Len() int { return len(w.byName) }

// safetensors layout, in full:
//
//	[8 bytes]  header length N, little-endian uint64
//	[N bytes]  JSON header: name -> {dtype, shape, data_offsets}
//	[rest]     tensor data, offsets relative to the end of the header
//
// That is the entire format. It is why this file is a hundred lines instead of
// the several thousand a pickle-based checkpoint reader would need.
type stHeader struct {
	DType   string `json:"dtype"`
	Shape   []int  `json:"shape"`
	Offsets []int  `json:"data_offsets"`
}

const maxHeaderBytes = 100 << 20

// LoadSafetensors reads a .safetensors file, converting every supported dtype
// to float32.
func LoadSafetensors(path string) (*Weights, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 8 {
		return nil, fmt.Errorf("%s: too short to be safetensors", path)
	}
	n := binary.LittleEndian.Uint64(raw[:8])
	if n > maxHeaderBytes || uint64(len(raw)) < 8+n {
		return nil, fmt.Errorf("%s: implausible header length %d", path, n)
	}

	var hdr map[string]json.RawMessage
	if err := json.Unmarshal(raw[8:8+n], &hdr); err != nil {
		return nil, fmt.Errorf("%s: parse header: %w", path, err)
	}
	body := raw[8+n:]

	w := &Weights{
		byName:  make(map[string]*Tensor, len(hdr)),
		skipped: map[string]string{},
	}
	for name, rawEntry := range hdr {
		if name == "__metadata__" {
			continue
		}
		var e stHeader
		if err := json.Unmarshal(rawEntry, &e); err != nil {
			return nil, fmt.Errorf("%s: tensor %q: %w", path, name, err)
		}
		if len(e.Offsets) != 2 {
			return nil, fmt.Errorf("%s: tensor %q has %d offsets", path, name, len(e.Offsets))
		}
		start, end := e.Offsets[0], e.Offsets[1]
		if start < 0 || end > len(body) || start > end {
			return nil, fmt.Errorf("%s: tensor %q offsets %d..%d out of range (%d bytes)",
				path, name, start, end, len(body))
		}
		data, err := decodeDType(e.DType, body[start:end])
		if err != nil {
			// An undecodable tensor is not an error by itself.
			//
			// Real checkpoints carry integer buffers alongside their weights:
			// all-MiniLM ships `embeddings.position_ids` as I64, which is just
			// [0,1,2,…] and is never read by the forward pass. Refusing to
			// load a model because of a tensor nothing uses would reject most
			// published checkpoints. If a tensor we actually need lands here,
			// the lookup that wants it reports the dtype precisely.
			w.skipped[name] = e.DType
			continue
		}
		count := 1
		for _, d := range e.Shape {
			count *= d
		}
		if len(data) != count {
			return nil, fmt.Errorf("%s: tensor %q shape %v needs %d values, got %d",
				path, name, e.Shape, count, len(data))
		}
		w.byName[name] = &Tensor{Name: name, Shape: e.Shape, Data: data}
	}
	if len(w.byName) == 0 {
		return nil, fmt.Errorf("%s: no tensors", path)
	}
	return w, nil
}

// decodeDType widens a stored dtype to float32.
//
// F16 and BF16 both appear in published checkpoints; converting on load costs
// memory but keeps the forward pass a single precision, which is worth far
// more than the bytes saved.
func decodeDType(dtype string, b []byte) ([]float32, error) {
	switch dtype {
	case "F32":
		if len(b)%4 != 0 {
			return nil, fmt.Errorf("F32 data is not a multiple of 4 bytes")
		}
		out := make([]float32, len(b)/4)
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
		}
		return out, nil

	case "F16":
		if len(b)%2 != 0 {
			return nil, fmt.Errorf("F16 data is not a multiple of 2 bytes")
		}
		out := make([]float32, len(b)/2)
		for i := range out {
			out[i] = float16to32(binary.LittleEndian.Uint16(b[i*2:]))
		}
		return out, nil

	case "BF16":
		if len(b)%2 != 0 {
			return nil, fmt.Errorf("BF16 data is not a multiple of 2 bytes")
		}
		out := make([]float32, len(b)/2)
		for i := range out {
			// bfloat16 is simply the top 16 bits of a float32.
			out[i] = math.Float32frombits(uint32(binary.LittleEndian.Uint16(b[i*2:])) << 16)
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported dtype %q (need F32, F16, or BF16)", dtype)
}

// float16to32 expands IEEE half precision.
func float16to32(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := (h >> 10) & 0x1f
	mant := uint32(h & 0x03ff)

	switch exp {
	case 0:
		if mant == 0 {
			return math.Float32frombits(sign) // signed zero
		}
		// Subnormal: renormalise into float32's exponent range.
		e := uint32(127 - 15 + 1)
		for mant&0x0400 == 0 {
			mant <<= 1
			e--
		}
		mant &= 0x03ff
		return math.Float32frombits(sign | e<<23 | mant<<13)
	case 0x1f:
		// Inf or NaN.
		return math.Float32frombits(sign | 0xff<<23 | mant<<13)
	default:
		return math.Float32frombits(sign | uint32(exp-15+127)<<23 | mant<<13)
	}
}

// WriteSafetensors serialises tensors, used to build fixtures for the
// self-check without shipping a model.
func WriteSafetensors(path string, tensors []*Tensor) error {
	hdr := map[string]stHeader{}
	offset := 0
	// Deterministic order so the file is reproducible.
	sorted := append([]*Tensor(nil), tensors...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	for _, t := range sorted {
		size := len(t.Data) * 4
		hdr[t.Name] = stHeader{DType: "F32", Shape: t.Shape, Offsets: []int{offset, offset + size}}
		offset += size
	}
	hj, err := json.Marshal(hdr)
	if err != nil {
		return err
	}

	buf := make([]byte, 8+len(hj)+offset)
	binary.LittleEndian.PutUint64(buf[:8], uint64(len(hj)))
	copy(buf[8:], hj)
	body := buf[8+len(hj):]
	pos := 0
	for _, t := range sorted {
		for _, v := range t.Data {
			binary.LittleEndian.PutUint32(body[pos:], math.Float32bits(v))
			pos += 4
		}
	}
	return os.WriteFile(path, buf, 0o644)
}
