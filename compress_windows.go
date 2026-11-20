//go:build windows

package main

// Recordings are kept as WAV (16 kHz, 16-bit, mic | PC sound: about 230 MB an hour) and
// sent as AAC in an .m4a (96 kbit/s: about 43 MB an hour), made with Windows' own encoder
// (Media Foundation) just before uploading. The two channels stay apart, so the server can
// still tell you from the others. If Windows can't encode it (N editions without the Media
// Feature Pack, say), the WAV is sent as before.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	mfplat                     = windows.NewLazySystemDLL("mfplat.dll")
	mfreadwrite                = windows.NewLazySystemDLL("mfreadwrite.dll")
	pMFStartup                 = mfplat.NewProc("MFStartup")
	pMFShutdown                = mfplat.NewProc("MFShutdown")
	pMFCreateMediaType         = mfplat.NewProc("MFCreateMediaType")
	pMFCreateMemoryBuffer      = mfplat.NewProc("MFCreateMemoryBuffer")
	pMFCreateSample            = mfplat.NewProc("MFCreateSample")
	pMFCreateSinkWriterFromURL = mfreadwrite.NewProc("MFCreateSinkWriterFromURL")
)

func mustGUID(s string) windows.GUID {
	g, err := windows.GUIDFromString(s)
	if err != nil {
		panic(err)
	}
	return g
}

var (
	mtMajorType       = mustGUID("{48eba18e-f8c9-4687-bf11-0a74c9f96a8f}") // MF_MT_MAJOR_TYPE
	mtSubtype         = mustGUID("{f7e34c9a-42e8-4714-b74b-cb29d72c35e5}") // MF_MT_SUBTYPE
	mtChannels        = mustGUID("{37e48bf5-645e-4c5b-89de-ada9e29b696a}") // MF_MT_AUDIO_NUM_CHANNELS
	mtSampleRate      = mustGUID("{5faeeae7-0290-4c31-9e8a-c534f68d9dba}") // MF_MT_AUDIO_SAMPLES_PER_SECOND
	mtBitsPerSample   = mustGUID("{f2deb57f-40fa-4764-aa33-ed4f2d1ff669}") // MF_MT_AUDIO_BITS_PER_SAMPLE
	mtAvgBytesPerSec  = mustGUID("{1aab75c8-cfef-451c-ab95-ac034b8e1731}") // MF_MT_AUDIO_AVG_BYTES_PER_SECOND
	mtBlockAlignment  = mustGUID("{322de230-9eeb-43bd-ab7a-ff412251541d}") // MF_MT_AUDIO_BLOCK_ALIGNMENT
	mediaTypeAudio    = mustGUID("{73647561-0000-0010-8000-00aa00389b71}") // MFMediaType_Audio
	audioFormatPCM    = mustGUID("{00000001-0000-0010-8000-00aa00389b71}") // MFAudioFormat_PCM
	audioFormatAAC    = mustGUID("{00001610-0000-0010-8000-00aa00389b71}") // MFAudioFormat_AAC
	errNotOurWav      = errors.New("not a meetrec recording (16-bit PCM WAV)")
	aacBytesPerSecond = uint32(12000) // 96 kbit/s, the encoder's lowest setting; plenty for speech
)

// com calls method idx of a COM object's vtable. Pointers passed as uintptr (out-parameters
// like &p) are moved to the heap by uintptrescapes: a goroutine stack can move while Windows
// writes into it.
//
//go:uintptrescapes
func com(obj unsafe.Pointer, idx int, args ...uintptr) error {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, idx*int(unsafe.Sizeof(uintptr(0)))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	if int32(r) < 0 {
		return fmt.Errorf("Media Foundation error 0x%08X", uint32(r))
	}
	return nil
}

func comRelease(obj unsafe.Pointer) {
	if obj != nil {
		_ = com(obj, 2)
	}
}

// IMFAttributes / IMFMediaType methods
const (
	attrSetUINT32 = 21
	attrSetGUID   = 24
)

func mediaType(attrs map[*windows.GUID]any) (unsafe.Pointer, error) {
	var mt unsafe.Pointer
	if r, _, _ := pMFCreateMediaType.Call(uintptr(unsafe.Pointer(&mt))); int32(r) < 0 {
		return nil, fmt.Errorf("MFCreateMediaType: 0x%08X", uint32(r))
	}
	for k, v := range attrs {
		var err error
		switch v := v.(type) {
		case windows.GUID:
			err = com(mt, attrSetGUID, uintptr(unsafe.Pointer(k)), uintptr(unsafe.Pointer(&v)))
		case uint32:
			err = com(mt, attrSetUINT32, uintptr(unsafe.Pointer(k)), uintptr(v))
		}
		if err != nil {
			comRelease(mt)
			return nil, err
		}
	}
	return mt, nil
}

// compressAudio makes dir/audio.m4a from dir/audio.wav (or reuses one made earlier) and
// returns its path. progress (may be nil) gets 0…1 as it goes.
func compressAudio(dir string, progress func(float64)) (string, error) {
	wav, out := filepath.Join(dir, "audio.wav"), filepath.Join(dir, "audio.m4a")
	wi, err := os.Stat(wav)
	if err != nil {
		return "", err
	}
	if oi, err := os.Stat(out); err == nil && oi.ModTime().After(wi.ModTime()) && oi.Size() > 0 {
		return out, nil
	}
	tmp := filepath.Join(dir, "audio.part.m4a") // the extension picks the MP4 container
	_ = os.Remove(tmp)
	if err := encodeAAC(wav, tmp, progress); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := checkM4A(tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, out); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return out, nil
}

// checkM4A: an MP4 file ("ftyp" box first) that isn't empty.
func checkM4A(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var h [8]byte
	info, _ := f.Stat()
	if _, err := io.ReadFull(f, h[:]); err != nil || string(h[4:8]) != "ftyp" || info.Size() < 1024 {
		return errors.New("the encoder made no usable .m4a")
	}
	return nil
}

func encodeAAC(wavPath, outPath string, progress func(float64)) (err error) {
	f, err := os.Open(wavPath)
	if err != nil {
		return err
	}
	defer f.Close()
	var h [44]byte
	if _, err := io.ReadFull(f, h[:]); err != nil || string(h[0:4]) != "RIFF" || string(h[8:12]) != "WAVE" {
		return errNotOurWav
	}
	channels := uint32(binary.LittleEndian.Uint16(h[22:24]))
	rate := binary.LittleEndian.Uint32(h[24:28])
	bits := binary.LittleEndian.Uint16(h[34:36])
	if binary.LittleEndian.Uint16(h[20:22]) != 1 || bits != 16 || channels < 1 || channels > 2 || rate == 0 {
		return errNotOurWav
	}

	// Media Foundation needs COM on this thread
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	switch err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); {
	case err == nil || err == syscall.Errno(1): // S_OK, S_FALSE (already, same kind)
		defer windows.CoUninitialize()
	case err == syscall.Errno(0x80010106): // RPC_E_CHANGED_MODE: this thread already has COM, use it
	default:
		return fmt.Errorf("CoInitializeEx: %w", err)
	}
	if err := mfplat.Load(); err != nil {
		return fmt.Errorf("Media Foundation isn't available: %w", err)
	}
	if r, _, _ := pMFStartup.Call(0x20070, 0); int32(r) < 0 { // MF_VERSION, MFSTARTUP_FULL
		return fmt.Errorf("MFStartup: 0x%08X", uint32(r))
	}
	defer pMFShutdown.Call()

	var writer unsafe.Pointer
	url, _ := windows.UTF16PtrFromString(outPath)
	if r, _, _ := pMFCreateSinkWriterFromURL.Call(uintptr(unsafe.Pointer(url)), 0, 0, uintptr(unsafe.Pointer(&writer))); int32(r) < 0 {
		return fmt.Errorf("MFCreateSinkWriterFromURL: 0x%08X", uint32(r))
	}
	defer comRelease(writer)

	// the AAC encoder takes 44.1 or 48 kHz: 16 kHz recordings go in at 48 kHz (×3)
	inRate, up := rate, 1
	if rate != 44100 && rate != 48000 {
		if 48000%rate != 0 {
			return fmt.Errorf("can't encode %d Hz audio", rate)
		}
		inRate, up = 48000, int(48000/rate)
	}
	outType, err := mediaType(map[*windows.GUID]any{
		&mtMajorType: mediaTypeAudio, &mtSubtype: audioFormatAAC, &mtBitsPerSample: uint32(16),
		&mtSampleRate: inRate, &mtChannels: channels, &mtAvgBytesPerSec: aacBytesPerSecond,
	})
	if err != nil {
		return err
	}
	defer comRelease(outType)
	var stream uint32
	if err := com(writer, 3, uintptr(outType), uintptr(unsafe.Pointer(&stream))); err != nil { // AddStream
		return fmt.Errorf("AAC encoder: %w", err)
	}
	block := channels * 2
	inType, err := mediaType(map[*windows.GUID]any{
		&mtMajorType: mediaTypeAudio, &mtSubtype: audioFormatPCM, &mtBitsPerSample: uint32(16),
		&mtSampleRate: inRate, &mtChannels: channels, &mtBlockAlignment: block, &mtAvgBytesPerSec: inRate * block,
	})
	if err != nil {
		return err
	}
	defer comRelease(inType)
	if err := com(writer, 4, uintptr(stream), uintptr(inType), 0); err != nil { // SetInputMediaType
		return fmt.Errorf("AAC encoder input: %w", err)
	}
	if err := com(writer, 5); err != nil { // BeginWriting
		return fmt.Errorf("BeginWriting: %w", err)
	}

	ups := make([]*upsampler, channels)
	for i := range ups {
		ups[i] = newUpsampler(up)
	}
	const secondsPerSample = 1
	in := make([]byte, int(rate)*int(block)*secondsPerSample)
	var written int64 // frames at inRate
	total := float64(1)
	if info, err := f.Stat(); err == nil && info.Size() > 44 {
		total = float64(info.Size() - 44)
	}
	read := 0
	for {
		n, rerr := io.ReadFull(f, in)
		n -= n % int(block)
		if n > 0 {
			pcm := interleave(in[:n], int(channels), ups)
			if err := writeSample(writer, stream, pcm, written, inRate, block); err != nil {
				return err
			}
			written += int64(len(pcm)) / int64(block)
			if read += n; progress != nil {
				progress(min(float64(read)/total, 1))
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if written == 0 {
		return errors.New("the recording is empty")
	}
	if err := com(writer, 11); err != nil { // Finalize
		return fmt.Errorf("Finalize: %w", err)
	}
	return nil
}

// interleave upsamples each channel of 16-bit little-endian PCM and interleaves it again.
func interleave(in []byte, channels int, ups []*upsampler) []byte {
	frames := len(in) / (2 * channels)
	if ups[0].factor == 1 {
		return in
	}
	chans := make([][]int16, channels)
	for c := range chans {
		x := make([]float32, frames)
		for i := 0; i < frames; i++ {
			x[i] = float32(int16(binary.LittleEndian.Uint16(in[2*(i*channels+c):])))
		}
		chans[c] = ups[c].process(x)
	}
	n := len(chans[0])
	out := make([]byte, 2*n*channels)
	for i := 0; i < n; i++ {
		for c := range chans {
			binary.LittleEndian.PutUint16(out[2*(i*channels+c):], uint16(chans[c][i]))
		}
	}
	return out
}

func writeSample(writer unsafe.Pointer, stream uint32, pcm []byte, at int64, rate, block uint32) error {
	var buf, sample unsafe.Pointer
	if r, _, _ := pMFCreateMemoryBuffer.Call(uintptr(len(pcm)), uintptr(unsafe.Pointer(&buf))); int32(r) < 0 {
		return fmt.Errorf("MFCreateMemoryBuffer: 0x%08X", uint32(r))
	}
	defer comRelease(buf)
	var p unsafe.Pointer
	if err := com(buf, 3, uintptr(unsafe.Pointer(&p)), 0, 0); err != nil { // Lock
		return err
	}
	copy(unsafe.Slice((*byte)(p), len(pcm)), pcm)
	_ = com(buf, 4)                                        // Unlock
	if err := com(buf, 6, uintptr(len(pcm))); err != nil { // SetCurrentLength
		return err
	}
	if r, _, _ := pMFCreateSample.Call(uintptr(unsafe.Pointer(&sample))); int32(r) < 0 {
		return fmt.Errorf("MFCreateSample: 0x%08X", uint32(r))
	}
	defer comRelease(sample)
	frames := int64(len(pcm)) / int64(block)
	hns := func(f int64) uintptr { return uintptr(f * 10_000_000 / int64(rate)) } // 100 ns units
	if err := com(sample, 42, uintptr(buf)); err != nil {                         // AddBuffer
		return err
	}
	_ = com(sample, 36, hns(at))                                             // SetSampleTime
	_ = com(sample, 38, hns(at+frames)-hns(at))                              // SetSampleDuration
	if err := com(writer, 6, uintptr(stream), uintptr(sample)); err != nil { // WriteSample
		return fmt.Errorf("WriteSample: %w", err)
	}
	return nil
}
