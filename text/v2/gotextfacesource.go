// Copyright 2023 The Ebitengine Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package text

import (
	"bytes"
	"io"
	"runtime"
	"sync"

	"github.com/go-text/typesetting/font"
	"github.com/go-text/typesetting/language"
	"github.com/go-text/typesetting/opentype/api"
	"github.com/go-text/typesetting/shaping"
)

type goTextOutputCacheKey struct {
	text         string
	direction    Direction
	sizeInPixels float64
	language     string
	script       string
	variations   string
	features     string
}

type glyph struct {
	shapingGlyph   *shaping.Glyph
	startIndex     int
	endIndex       int
	scaledSegments []api.Segment
}

type goTextOutputCacheValue struct {
	output shaping.Output
	glyphs []glyph
	atime  int64
}

// GoTextFaceSource is a source of a GoTextFace. This can be shared by multiple GoTextFace objects.
type GoTextFaceSource struct {
	f  font.Face
	id uint64

	outputCache map[goTextOutputCacheKey]*goTextOutputCacheValue

	m sync.Mutex
}

func toFontResource(source io.ReadSeeker) (font.Resource, error) {
	// font.Resource has io.ReaderAt in addition to io.ReadSeeker.
	// If source has it, use it as it is.
	if s, ok := source.(font.Resource); ok {
		return s, nil
	}

	// Read all the bytes and convert this to bytes.Reader.
	// This is a very rough solution, but it works.
	// TODO: Implement io.ReaderAt in a more efficient way.
	bs, err := io.ReadAll(source)
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(bs), nil
}

// NewGoTextFaceSource parses an OpenType or TrueType font and returns a GoTextFaceSource object.
func NewGoTextFaceSource(source io.ReadSeeker) (*GoTextFaceSource, error) {
	src, err := toFontResource(source)
	if err != nil {
		return nil, err
	}

	f, err := font.ParseTTF(src)
	if err != nil {
		return nil, err
	}

	s := &GoTextFaceSource{
		f:  f,
		id: nextUniqueID(),
	}
	runtime.SetFinalizer(s, finalizeGoTextFaceSource)
	return s, nil
}

// NewGoTextFaceSourcesFromCollection parses an OpenType or TrueType font collection and returns a slice of GoTextFaceSource objects.
func NewGoTextFaceSourcesFromCollection(source io.ReadSeeker) ([]*GoTextFaceSource, error) {
	src, err := toFontResource(source)
	if err != nil {
		return nil, err
	}

	fs, err := font.ParseTTC(src)
	if err != nil {
		return nil, err
	}

	sources := make([]*GoTextFaceSource, len(fs))
	for i, f := range fs {
		s := &GoTextFaceSource{
			f:  f,
			id: nextUniqueID(),
		}
		runtime.SetFinalizer(s, finalizeGoTextFaceSource)
		sources[i] = s
	}
	return sources, nil
}

func finalizeGoTextFaceSource(source *GoTextFaceSource) {
	runtime.SetFinalizer(source, nil)
	theGlyphImageCache.clear(func(key faceCacheKey) bool {
		return key.id == source.id
	})
}

func (g *GoTextFaceSource) shape(text string, face *GoTextFace) (shaping.Output, []glyph) {
	g.m.Lock()
	defer g.m.Unlock()

	key := face.outputCacheKey(text)
	if out, ok := g.outputCache[key]; ok {
		out.atime = now()
		return out.output, out.glyphs
	}

	g.f.SetVariations(face.variations)
	runes := []rune(text)
	input := shaping.Input{
		Text:         runes,
		RunStart:     0,
		RunEnd:       len(runes),
		Direction:    face.diDirection(),
		Face:         face.Source.f,
		FontFeatures: face.features,
		Size:         float64ToFixed26_6(face.SizeInPixels),
		Script:       face.gScript(),
		Language:     language.Language(face.Language.String()),
	}
	out := (&shaping.HarfbuzzShaper{}).Shape(input)
	if g.outputCache == nil {
		g.outputCache = map[goTextOutputCacheKey]*goTextOutputCacheValue{}
	}

	var indices []int
	for i := range text {
		indices = append(indices, i)
	}
	indices = append(indices, len(text))

	gs := make([]glyph, len(out.Glyphs))
	for i, gl := range out.Glyphs {
		gl := gl
		var segs []api.Segment
		switch data := g.f.GlyphData(gl.GlyphID).(type) {
		case api.GlyphOutline:
			segs = data.Segments
		case api.GlyphSVG:
			segs = data.Outline.Segments
		case api.GlyphBitmap:
			if data.Outline != nil {
				segs = data.Outline.Segments
			}
		}

		scaledSegs := make([]api.Segment, len(segs))
		scale := float32(g.scale(fixed26_6ToFloat64(out.Size)))
		for i, seg := range segs {
			scaledSegs[i] = seg
			for j := range seg.Args {
				scaledSegs[i].Args[j].X *= scale
				scaledSegs[i].Args[j].Y *= scale
				scaledSegs[i].Args[j].Y *= -1
			}
		}

		gs[i] = glyph{
			shapingGlyph:   &gl,
			startIndex:     indices[gl.ClusterIndex],
			endIndex:       indices[gl.ClusterIndex+gl.RuneCount],
			scaledSegments: scaledSegs,
		}
	}
	g.outputCache[key] = &goTextOutputCacheValue{
		output: out,
		glyphs: gs,
		atime:  now(),
	}

	const cacheSoftLimit = 512
	if len(g.outputCache) > cacheSoftLimit {
		for key, e := range g.outputCache {
			// 60 is an arbitrary number.
			if e.atime >= now()-60 {
				continue
			}
			delete(g.outputCache, key)
		}
	}

	return out, gs
}

func (g *GoTextFaceSource) scale(sizeInPixels float64) float64 {
	return sizeInPixels / float64(g.f.Upem())
}