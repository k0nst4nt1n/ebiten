// Copyright 2014 Hajime Hoshi
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

package ebiten

import (
	"fmt"
	"math"
	"sync"

	"github.com/hajimehoshi/ebiten/internal/buffered"
	"github.com/hajimehoshi/ebiten/internal/clock"
	"github.com/hajimehoshi/ebiten/internal/driver"
	"github.com/hajimehoshi/ebiten/internal/graphicscommand"
	"github.com/hajimehoshi/ebiten/internal/hooks"
	"github.com/hajimehoshi/ebiten/internal/shareable"
)

func init() {
	shareable.SetGraphicsDriver(graphicsDriver())
	graphicscommand.SetGraphicsDriver(graphicsDriver())
}

type defaultGame struct {
	update func(screen *Image) error
	width  int
	height int
}

func (d *defaultGame) Update(screen *Image) error {
	return d.update(screen)
}

func (d *defaultGame) Layout(outsideWidth, outsideHeight int) (screenWidth, screenHeight int) {
	// Ignore the outside size.
	return d.width, d.height
}

func (d *defaultGame) setScreenSize(width, height int) {
	d.width = width
	d.height = height
}

func newUIContext(game Game, width, height int, scaleForWindow float64) *uiContext {
	return &uiContext{
		game:           game,
		scaleForWindow: scaleForWindow,
	}
}

type uiContext struct {
	game           Game
	offscreen      *Image
	screen         *Image
	screenScale    float64
	scaleForWindow float64
	offsetX        float64
	offsetY        float64

	outsideSizeUpdated bool
	outsideWidth       float64
	outsideHeight      float64

	m sync.Mutex
}

var theUIContext *uiContext

func (c *uiContext) setScaleForWindow(scale float64) {
	g, ok := c.game.(*defaultGame)
	if !ok {
		panic("ebiten: setScaleForWindow must be called when Run is used")
	}
	w, h := g.width, g.height
	c.m.Lock()
	c.scaleForWindow = scale
	uiDriver().SetWindowSize(int(float64(w)*scale), int(float64(h)*scale))
	c.m.Unlock()
}

func (c *uiContext) getScaleForWindow() float64 {
	if _, ok := c.game.(*defaultGame); !ok {
		panic("ebiten: getScaleForWindow must be called when Run is used")
	}
	c.m.Lock()
	s := c.scaleForWindow
	c.m.Unlock()
	return s
}

func (c *uiContext) SetScreenSize(width, height int) {
	c.m.Lock()
	defer c.m.Unlock()

	g, ok := c.game.(*defaultGame)
	if !ok {
		panic("ebiten: SetScreenSize must be called when Run is used")
	}
	g.setScreenSize(width, height)
	s := c.scaleForWindow
	uiDriver().SetWindowSize(int(float64(width)*s), int(float64(height)*s))
}

func (c *uiContext) Layout(outsideWidth, outsideHeight float64) {
	c.outsideSizeUpdated = true
	c.outsideWidth = outsideWidth
	c.outsideHeight = outsideHeight
}

func (c *uiContext) updateOffscreen() {
	sw, sh := c.game.Layout(int(c.outsideWidth), int(c.outsideHeight))

	if c.offscreen != nil && !c.outsideSizeUpdated {
		if w, h := c.offscreen.Size(); w == sw && h == sh {
			return
		}
	}
	c.outsideSizeUpdated = false

	if c.screen != nil {
		_ = c.screen.Dispose()
		c.screen = nil
	}

	if c.offscreen != nil {
		if w, h := c.offscreen.Size(); w != sw || h != sh {
			_ = c.offscreen.Dispose()
			c.offscreen = nil
		}
	}
	if c.offscreen == nil {
		c.offscreen = newImage(sw, sh, FilterDefault, true)
	}
	c.SetScreenSize(sw, sh)

	// TODO: This is duplicated with mobile/ebitenmobileview/funcs.go. Refactor this.
	d := uiDriver().DeviceScaleFactor()
	c.screen = newScreenFramebufferImage(int(c.outsideWidth*d), int(c.outsideHeight*d))

	scaleX := c.outsideWidth / float64(sw) * d
	scaleY := c.outsideHeight / float64(sh) * d
	c.screenScale = math.Min(scaleX, scaleY)
	if uiDriver().CanHaveWindow() && !uiDriver().IsFullscreen() {
		c.setScaleForWindow(c.screenScale / d)
	}

	width := float64(sw) * c.screenScale
	height := float64(sh) * c.screenScale
	c.offsetX = (c.outsideWidth*d - width) / 2
	c.offsetY = (c.outsideHeight*d - height) / 2
}

func (c *uiContext) Update(afterFrameUpdate func()) error {
	updateCount := clock.Update(MaxTPS())

	// TODO: If updateCount is 0 and vsync is disabled, swapping buffers can be skipped.

	if err := buffered.BeginFrame(); err != nil {
		return err
	}

	for i := 0; i < updateCount; i++ {
		c.updateOffscreen()

		// Mipmap images should be disposed by Clear.
		c.offscreen.Clear()

		setDrawingSkipped(i < updateCount-1)

		if err := hooks.RunBeforeUpdateHooks(); err != nil {
			return err
		}
		if err := c.game.Update(c.offscreen); err != nil {
			return err
		}
		uiDriver().Input().ResetForFrame()
		afterFrameUpdate()
	}

	// This clear is needed for fullscreen mode or some mobile platforms (#622).
	c.screen.Clear()

	op := &DrawImageOptions{}

	switch vd := graphicsDriver().VDirection(); vd {
	case driver.VDownward:
		// c.screen is special: its Y axis is down to up,
		// and the origin point is lower left.
		op.GeoM.Scale(c.screenScale, -c.screenScale)
		_, h := c.offscreen.Size()
		op.GeoM.Translate(0, float64(h)*c.screenScale)
	case driver.VUpward:
		op.GeoM.Scale(c.screenScale, c.screenScale)
	default:
		panic(fmt.Sprintf("ebiten: invalid v-direction: %d", vd))
	}

	op.GeoM.Translate(c.offsetX, c.offsetY)
	op.CompositeMode = CompositeModeCopy

	// filterScreen works with >=1 scale, but does not well with <1 scale.
	// Use regular FilterLinear instead so far (#669).
	if c.screenScale >= 1 {
		op.Filter = filterScreen
	} else {
		op.Filter = FilterLinear
	}
	_ = c.screen.DrawImage(c.offscreen, op)

	if err := buffered.EndFrame(); err != nil {
		return err
	}
	return nil
}

func (c *uiContext) AdjustPosition(x, y float64) (float64, float64) {
	d := uiDriver().DeviceScaleFactor()
	return (x*d - c.offsetX) / c.screenScale, (y*d - c.offsetY) / c.screenScale
}
