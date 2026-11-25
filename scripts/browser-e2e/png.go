package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"image/color"
	"image/png"
	"io"
	"os"
)

func check(r *bufio.Reader) error {
	header, err := r.Peek(24)
	if err != nil {
		return err
	}
	if string(header[12:16]) != "IHDR" || binary.BigEndian.Uint32(header[16:20]) != 800 || binary.BigEndian.Uint32(header[20:24]) != 480 {
		return errors.New("expected an 800×480 PNG")
	}
	img, err := png.Decode(r)
	if err != nil {
		return err
	}
	if _, err := r.ReadByte(); !errors.Is(err, io.EOF) {
		return errors.New("trailing screenshot data")
	}
	for _, p := range []struct {
		x, y int
		want color.NRGBA
	}{
		{8, 8, color.NRGBA{0xed, 0xf2, 0xf7, 0xff}},
		{154, 240, color.NRGBA{0x14, 0x81, 0x6a, 0xff}},
		{166, 240, color.NRGBA{0xff, 0xff, 0xff, 0xff}},
	} {
		if got := color.NRGBAModel.Convert(img.At(p.x, p.y)); got != p.want {
			return fmt.Errorf("fixture pixel (%d,%d): got %v, want %v", p.x, p.y, got, p.want)
		}
	}
	return nil
}

func main() {
	if err := check(bufio.NewReader(os.Stdin)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
