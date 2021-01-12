// Copyright 2021 The Ebiten Authors
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

package go2cpp

import (
	"io"
	"runtime"
	"sync"
	"syscall/js"
)

type Context struct {
	v js.Value
}

func NewContext(sampleRate int) *Context {
	v := js.Global().Get("go2cpp").Call("createAudio", sampleRate, 2, 2, 8192)
	return &Context{
		v: v,
	}
}

func (c *Context) NewPlayer(r io.Reader) *Player {
	cond := sync.NewCond(&sync.Mutex{})
	onwritten := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		cond.Signal()
		return nil
	})
	p := &Player{
		context:   c,
		src:       r,
		volume:    1,
		cond:      cond,
		onWritten: onwritten,
	}
	runtime.SetFinalizer(p, (*Player).Close)
	return p
}

func (c *Context) Close() error {
	return nil
}

type playerState int

const (
	playerStatePaused playerState = iota
	playerStatePlaying
	playerStateClosed
)

type Player struct {
	context *Context
	src     io.Reader
	v       js.Value
	state   playerState
	volume  float64
	cond    *sync.Cond
	err     error

	onWritten js.Func
}

func (p *Player) Pause() {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	if p.state == playerStateClosed {
		return
	}
	if !p.v.Truthy() {
		return
	}

	p.v.Call("pause")
	p.state = playerStatePaused
	p.cond.Signal()
}

func (p *Player) Play() {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	if p.state == playerStateClosed {
		return
	}
	if !p.v.Truthy() {
		p.v = p.context.v.Call("createPlayer", p.onWritten)
		p.v.Set("volume", p.volume)
		go p.loop()
	}

	p.v.Call("play")
	p.state = playerStatePlaying
	p.cond.Signal()
}

func (p *Player) Reset() {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	if p.state == playerStateClosed {
		return
	}
	if !p.v.Truthy() {
		return
	}

	p.v.Call("close", true)
	p.v = js.Undefined()
	p.cond.Signal()
}

func (p *Player) Volume() float64 {
	if !p.v.Truthy() {
		return p.volume
	}
	return p.v.Get("volume").Float()
}

func (p *Player) SetVolume(volume float64) {
	if !p.v.Truthy() {
		return
	}
	p.v.Set("volume", volume)
	p.volume = volume
}

func (p *Player) Close() error {
	runtime.SetFinalizer(p, nil)
	return p.close(true)
}

func (p *Player) close(remove bool) error {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	if p.state == playerStateClosed {
		return p.err
	}

	p.v.Call("close", false)
	p.v = js.Undefined()
	if remove {
		p.state = playerStateClosed
		p.onWritten.Release()
	} else {
		p.state = playerStatePaused
	}
	p.cond.Signal()
	return p.err
}

func (p *Player) setError(err error) {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	if p.state != playerStateClosed && p.v.Truthy() {
		p.v.Call("close", true)
		p.v = js.Undefined()
	}
	p.err = err
	p.state = playerStateClosed
	p.cond.Signal()
}

func (p *Player) waitUntilUnpaused() bool {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	for p.v.Truthy() && (p.state == playerStatePaused || (p.state == playerStatePlaying && !p.v.Call("isWritable").Bool())) {
		p.cond.Wait()
	}
	return p.v.Truthy() && p.state == playerStatePlaying
}

func (p *Player) write(dst js.Value, src []byte) {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	if p.state == playerStateClosed {
		return
	}
	if !p.v.Truthy() {
		return
	}

	js.CopyBytesToJS(dst, src)
	p.v.Call("write", dst, len(src))
}

func (p *Player) loop() {
	const size = 4096

	buf := make([]byte, size)
	dst := js.Global().Get("Uint8Array").New(size)

	for {
		if !p.waitUntilUnpaused() {
			return
		}

		n, err := p.src.Read(buf)
		if err != nil && err != io.EOF {
			p.setError(err)
			return
		}
		if n > 0 {
			p.write(dst, buf[:n])
		}

		if err == io.EOF {
			p.close(false)
			return
		}
	}
}
