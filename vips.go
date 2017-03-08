package bimg

/*
#cgo pkg-config: vips
#include "vips.h"
*/
import "C"

import (
	"errors"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	d "github.com/tj/go-debug"
)

// debug is internally used to
var debug = d.Debug("bimg")

// VipsVersion exposes the current libvips semantic version
const VipsVersion = string(C.VIPS_VERSION)

// HasMagickSupport exposes if the current libvips compilation
// supports libmagick bindings.
const HasMagickSupport = int(C.VIPS_MAGICK_SUPPORT) == 1

const (
	maxCacheMem  = 100 * 1024 * 1024
	maxCacheSize = 500
)

var (
	m           sync.Mutex
	initialized bool
)

// VipsMemoryInfo represents the memory stats provided by libvips.
type VipsMemoryInfo struct {
	Memory          int64
	MemoryHighwater int64
	Allocations     int64
}

// vipsSaveOptions represents the internal option used to talk with libvips.
type vipsSaveOptions struct {
	Quality        int
	Compression    int
	Type           ImageType
	Interlace      bool
	NoProfile      bool
	Interpretation Interpretation
}

type vipsWatermarkOptions struct {
	Width       C.int
	DPI         C.int
	Margin      C.int
	NoReplicate C.int
	Opacity     C.float
	Background  [3]C.double
}

type vipsWatermarkTextOptions struct {
	Text *C.char
	Font *C.char
}

func init() {
	Initialize()
}

// Initialize is used to explicitly start libvips in thread-safe way.
// Only call this function if you have previously turned off libvips.
func Initialize() {
	if C.VIPS_MAJOR_VERSION <= 7 && C.VIPS_MINOR_VERSION < 40 {
		panic("unsupported libvips version!")
	}

	m.Lock()
	runtime.LockOSThread()
	defer m.Unlock()
	defer runtime.UnlockOSThread()

	err := C.vips_init(C.CString("bimg"))
	if err != 0 {
		panic("unable to start vips!")
	}

	// Set libvips cache params
	C.vips_cache_set_max_mem(maxCacheMem)
	C.vips_cache_set_max(maxCacheSize)

	// Define a custom thread concurrency limit in libvips (this may generate thread-unsafe issues)
	// See: https://github.com/jcupitt/libvips/issues/261#issuecomment-92850414
	if os.Getenv("VIPS_CONCURRENCY") == "" {
		C.vips_concurrency_set(1)
	}

	// Enable libvips cache tracing
	if os.Getenv("VIPS_TRACE") != "" {
		C.vips_enable_cache_set_trace()
	}

	initialized = true
}

// Shutdown is used to shutdown libvips in a thread-safe way.
// You can call this to drop caches as well.
// If libvips was already initialized, the function is no-op
func Shutdown() {
	m.Lock()
	defer m.Unlock()

	if initialized {
		C.vips_shutdown()
		initialized = false
	}
}

// VipsDebugInfo outputs to stdout libvips collected data. Useful for debugging.
func VipsDebugInfo() {
	C.im__print_all()
}

// VipsMemory gets memory info stats from libvips (cache size, memory allocs...)
func VipsMemory() VipsMemoryInfo {
	return VipsMemoryInfo{
		Memory:          int64(C.vips_tracked_get_mem()),
		MemoryHighwater: int64(C.vips_tracked_get_mem_highwater()),
		Allocations:     int64(C.vips_tracked_get_allocs()),
	}
}

func vipsExifOrientation(image *C.VipsImage) int {
	return int(C.vips_exif_orientation(image))
}

func vipsHasAlpha(image *C.VipsImage) bool {
	return int(C.has_alpha_channel(image)) > 0
}

func vipsHasProfile(image *C.VipsImage) bool {
	return int(C.has_profile_embed(image)) > 0
}

func vipsWindowSize(name string) float64 {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	return float64(C.interpolator_window_size(cname))
}

func vipsSpace(image *C.VipsImage) string {
	return C.GoString(C.vips_enum_nick_bridge(image))
}

func vipsRotate(image *C.VipsImage, angle Angle) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(image))

	err := C.vips_rotate(image, &out, C.int(angle))
	if err != 0 {
		return nil, catchVipsError()
	}

	return out, nil
}

func vipsFlip(image *C.VipsImage, direction Direction) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(image))

	err := C.vips_flip_bridge(image, &out, C.int(direction))
	if err != 0 {
		return nil, catchVipsError()
	}

	return out, nil
}

func vipsZoom(image *C.VipsImage, zoom int) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(image))

	err := C.vips_zoom_bridge(image, &out, C.int(zoom), C.int(zoom))
	if err != 0 {
		return nil, catchVipsError()
	}

	return out, nil
}

func vipsWatermark(image *C.VipsImage, w Watermark) (*C.VipsImage, error) {
	var out *C.VipsImage

	// Defaults
	noReplicate := 0
	if w.NoReplicate {
		noReplicate = 1
	}

	text := C.CString(w.Text)
	font := C.CString(w.Font)
	background := [3]C.double{C.double(w.Background.R), C.double(w.Background.G), C.double(w.Background.B)}

	textOpts := vipsWatermarkTextOptions{text, font}
	opts := vipsWatermarkOptions{C.int(w.Width), C.int(w.DPI), C.int(w.Margin), C.int(noReplicate), C.float(w.Opacity), background}

	defer C.free(unsafe.Pointer(text))
	defer C.free(unsafe.Pointer(font))

	err := C.vips_watermark(image, &out, (*C.WatermarkTextOptions)(unsafe.Pointer(&textOpts)), (*C.WatermarkOptions)(unsafe.Pointer(&opts)))
	if err != 0 {
		return nil, catchVipsError()
	}

	return out, nil
}

func vipsRead(buf []byte) (*C.VipsImage, ImageType, error) {
	var image *C.VipsImage
	imageType := vipsImageType(buf)

	if imageType == UNKNOWN {
		return nil, UNKNOWN, errors.New("Unsupported image format")
	}

	length := C.size_t(len(buf))
	imageBuf := unsafe.Pointer(&buf[0])

	err := C.vips_init_image(imageBuf, length, C.int(imageType), &image)
	if err != 0 {
		return nil, UNKNOWN, catchVipsError()
	}

	return image, imageType, nil
}

func vipsColourspaceIsSupportedBuffer(buf []byte) (bool, error) {
	image, _, err := vipsRead(buf)
	if err != nil {
		return false, err
	}
	C.g_object_unref(C.gpointer(image))
	return vipsColourspaceIsSupported(image), nil
}

func vipsColourspaceIsSupported(image *C.VipsImage) bool {
	return int(C.vips_colourspace_issupported_bridge(image)) == 1
}

func vipsInterpretationBuffer(buf []byte) (Interpretation, error) {
	image, _, err := vipsRead(buf)
	if err != nil {
		return InterpretationError, err
	}
	C.g_object_unref(C.gpointer(image))
	return vipsInterpretation(image), nil
}

func vipsInterpretation(image *C.VipsImage) Interpretation {
	return Interpretation(C.vips_image_guess_interpretation_bridge(image))
}

func vipsFlattenBackground(image *C.VipsImage, background Color) (*C.VipsImage, error) {
	var outImage *C.VipsImage

	backgroundC := [3]C.double{
		C.double(background.R),
		C.double(background.G),
		C.double(background.B),
	}

	if vipsHasAlpha(image) {
		err := C.vips_flatten_background_brigde(image, &outImage, (*C.double)(&backgroundC[0]))
		if int(err) != 0 {
			return nil, catchVipsError()
		}
		C.g_object_unref(C.gpointer(image))
		image = outImage
	}

	return image, nil
}

func vipsPreSave(image *C.VipsImage, o *vipsSaveOptions) (*C.VipsImage, error) {
	// Remove ICC profile metadata
	if o.NoProfile {
		C.remove_profile(image)
	}

	// Use a default interpretation and cast it to C type
	if o.Interpretation == 0 {
		o.Interpretation = InterpretationSRGB
	}
	interpretation := C.VipsInterpretation(o.Interpretation)

	// Apply the proper colour space
	var outImage *C.VipsImage
	if vipsColourspaceIsSupported(image) {
		err := C.vips_colourspace_bridge(image, &outImage, interpretation)
		if int(err) != 0 {
			return nil, catchVipsError()
		}
		image = outImage
	}

	return image, nil
}

func vipsSave(image *C.VipsImage, o vipsSaveOptions) ([]byte, error) {
	defer C.g_object_unref(C.gpointer(image))

	tmpImage, err := vipsPreSave(image, &o)
	if err != nil {
		return nil, err
	}
	defer C.g_object_unref(C.gpointer(tmpImage))

	length := C.size_t(0)
	saveErr := C.int(0)
	interlace := C.int(boolToInt(o.Interlace))
	quality := C.int(o.Quality)

	var ptr unsafe.Pointer
	switch o.Type {
	case WEBP:
		saveErr = C.vips_webpsave_bridge(tmpImage, &ptr, &length, 1, quality)
		break
	case PNG:
		saveErr = C.vips_pngsave_bridge(tmpImage, &ptr, &length, 1, C.int(o.Compression), quality, interlace)
		break
	default:
		saveErr = C.vips_jpegsave_bridge(tmpImage, &ptr, &length, 1, quality, interlace)
		break
	}

	if int(saveErr) != 0 {
		return nil, catchVipsError()
	}

	buf := C.GoBytes(ptr, C.int(length))

	// Clean up
	C.g_free(C.gpointer(ptr))
	C.vips_error_clear()

	return buf, nil
}

func getImageBuffer(image *C.VipsImage) ([]byte, error) {
	var ptr unsafe.Pointer

	length := C.size_t(0)
	interlace := C.int(0)
	quality := C.int(100)

	err := C.int(0)
	err = C.vips_jpegsave_bridge(image, &ptr, &length, 1, quality, interlace)
	if int(err) != 0 {
		return nil, catchVipsError()
	}

	defer C.g_free(C.gpointer(ptr))
	defer C.vips_error_clear()

	return C.GoBytes(ptr, C.int(length)), nil
}

func vipsExtract(image *C.VipsImage, left, top, width, height int) (*C.VipsImage, error) {
	var buf *C.VipsImage
	defer C.g_object_unref(C.gpointer(image))

	if width > MaxSize || height > MaxSize {
		return nil, errors.New("Maximum image size exceeded")
	}

	top, left = max(top), max(left)
	err := C.vips_extract_area_bridge(image, &buf, C.int(left), C.int(top), C.int(width), C.int(height))
	if err != 0 {
		return nil, catchVipsError()
	}

	return buf, nil
}

func vipsShrinkJpeg(buf []byte, input *C.VipsImage, shrink int) (*C.VipsImage, error) {
	var image *C.VipsImage
	var ptr = unsafe.Pointer(&buf[0])
	defer C.g_object_unref(C.gpointer(input))

	err := C.vips_jpegload_buffer_shrink(ptr, C.size_t(len(buf)), &image, C.int(shrink))
	if err != 0 {
		return nil, catchVipsError()
	}

	return image, nil
}

func vipsShrink(input *C.VipsImage, shrink int) (*C.VipsImage, error) {
	var image *C.VipsImage
	defer C.g_object_unref(C.gpointer(input))

	err := C.vips_shrink_bridge(input, &image, C.double(float64(shrink)), C.double(float64(shrink)))
	if err != 0 {
		return nil, catchVipsError()
	}

	return image, nil
}

func vipsEmbed(input *C.VipsImage, left int, top int, width int, height int, extend Extend) (*C.VipsImage, error) {
	var image *C.VipsImage
	defer C.g_object_unref(C.gpointer(input))

	if extend > 5 {
		extend = ExtendBackground
	}

	err := C.vips_embed_bridge(input, &image, C.int(left), C.int(top), C.int(width), C.int(height), C.int(extend))
	if err != 0 {
		return nil, catchVipsError()
	}

	return image, nil
}

func vipsInsert(main *C.VipsImage, sub *C.VipsImage, left, top int) (*C.VipsImage, error) {
	var image *C.VipsImage
	defer C.g_object_unref(C.gpointer(main))
	defer C.g_object_unref(C.gpointer(sub))

	err := C.vips_insert_bridge(main, sub, &image, C.int(left), C.int(top))
	if err != 0 {
		return nil, catchVipsError()
	}

	return image, nil
}

func vipsAffine(input *C.VipsImage, residualx, residualy float64, i Interpolator) (*C.VipsImage, error) {
	var image *C.VipsImage
	cstring := C.CString(i.String())
	interpolator := C.vips_interpolate_new(cstring)

	defer C.free(unsafe.Pointer(cstring))
	defer C.g_object_unref(C.gpointer(input))
	defer C.g_object_unref(C.gpointer(interpolator))

	err := C.vips_affine_interpolator(input, &image, C.double(residualx), 0, 0, C.double(residualy), interpolator)
	if err != 0 {
		return nil, catchVipsError()
	}

	return image, nil
}

func vipsImageType(bytes []byte) ImageType {
	if len(bytes) == 0 {
		return UNKNOWN
	}

	if bytes[0] == 0x89 && bytes[1] == 0x50 && bytes[2] == 0x4E && bytes[3] == 0x47 {
		return PNG
	}
	if bytes[0] == 0xFF && bytes[1] == 0xD8 && bytes[2] == 0xFF {
		return JPEG
	}
	if bytes[8] == 0x57 && bytes[9] == 0x45 && bytes[10] == 0x42 && bytes[11] == 0x50 {
		return WEBP
	}
	if (bytes[0] == 0x49 && bytes[1] == 0x49 && bytes[2] == 0x2A && bytes[3] == 0x0) ||
		(bytes[0] == 0x4D && bytes[1] == 0x4D && bytes[2] == 0x0 && bytes[3] == 0x2A) {
		return TIFF
	}
	if HasMagickSupport && strings.HasSuffix(readImageType(bytes), "MagickBuffer") {
		return MAGICK
	}

	return UNKNOWN
}

func readImageType(buf []byte) string {
	length := C.size_t(len(buf))
	imageBuf := unsafe.Pointer(&buf[0])
	load := C.vips_foreign_find_load_buffer(imageBuf, length)
	defer C.free(imageBuf)
	return C.GoString(load)
}

func catchVipsError() error {
	s := C.GoString(C.vips_error_buffer())
	C.vips_error_clear()
	C.vips_thread_shutdown()
	return errors.New(s)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func vipsGaussianBlur(image *C.VipsImage, o GaussianBlur) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(image))

	err := C.vips_gaussblur_bridge(image, &out, C.double(o.Sigma), C.double(o.MinAmpl))
	if err != 0 {
		return nil, catchVipsError()
	}
	return out, nil
}

func vipsSharpen(image *C.VipsImage, o Sharpen) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(image))

	err := C.vips_sharpen_bridge(image, &out, C.int(o.Radius), C.double(o.X1), C.double(o.Y2), C.double(o.Y3), C.double(o.M1), C.double(o.M2))
	if err != 0 {
		return nil, catchVipsError()
	}
	return out, nil
}

func vipsExtractBand(image *C.VipsImage, band, numberOfBands int) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(image))

	err := C.vips_extract_band_bridge(image, &out, C.int(band), C.int(numberOfBands))
	if err != 0 {
		return nil, catchVipsError()
	}
	return out, nil

}

func vipsLinear1(image *C.VipsImage, a, b float64) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(image))

	err := C.vips_linear1_bridge(image, &out, C.double(a), C.double(b))
	if err != 0 {
		return nil, catchVipsError()
	}
	return out, nil
}

func vipsBlack(width, height, bands int) (*C.VipsImage, error) {
	var out *C.VipsImage

	err := C.vips_black_bridge(&out, C.int(width), C.int(height), C.int(bands))
	if err != 0 {
		return nil, catchVipsError()
	}
	return out, nil
}

func vipsAdd(left, right *C.VipsImage) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(left))
	defer C.g_object_unref(C.gpointer(right))

	err := C.vips_add_bridge(left, right, &out)
	if err != 0 {
		return nil, catchVipsError()
	}
	return out, nil
}

func vipsMultiply(left, right *C.VipsImage) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(left))
	defer C.g_object_unref(C.gpointer(right))

	err := C.vips_multiply_bridge(left, right, &out)
	if err != 0 {
		return nil, catchVipsError()
	}
	return out, nil
}

func vipsDivide(left, right *C.VipsImage) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(left))
	defer C.g_object_unref(C.gpointer(right))

	err := C.vips_divide_bridge(left, right, &out)
	if err != 0 {
		return nil, catchVipsError()
	}
	return out, nil
}

func vipsIthenelse(cond, in1, in2 *C.VipsImage, blend bool) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(cond))
	defer C.g_object_unref(C.gpointer(in1))
	defer C.g_object_unref(C.gpointer(in2))

	err := C.vips_ifthenelse_bridge(cond, in1, in2, &out, C.int(boolToInt(blend)))
	if err != 0 {
		return nil, catchVipsError()
	}
	return out, nil
}

func vipsBandjoin2(in1, in2 *C.VipsImage) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(in1))
	defer C.g_object_unref(C.gpointer(in2))

	err := C.vips_bandjoin2_bridge(in1, in2, &out)
	if err != 0 {
		return nil, catchVipsError()
	}
	return out, nil
}

func max(x int) int {
	return int(math.Max(float64(x), 0))
}
