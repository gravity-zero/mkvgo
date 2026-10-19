package commands

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mp4"
)

// The hand-written parseHLSFlags before CLI-03, to check the FlagSet one accepts the same arguments the same way.
func oldParseHLSFlags(args []string) hlsFlags {
	var f hlsFlags
	var keyHex, keyURI string
	var rotateSegs int
	var cencScheme, cencKeyHex, cencKIDHex, cencIVHex, cencKeyURI string
	var keyFile, cencKeyFile string
	var cencInline bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o":
			f.outDir = needValue(args, &i)
		case "-segment", "--segment":
			// Segment length in seconds, the HLS convention.
			needValue(args, &i)
			secs, err := strconv.ParseFloat(args[i], 64)
			if err != nil || !(secs >= 0.001 && secs <= 86400) {
				Fatal(fmt.Sprintf("invalid -segment duration %q (seconds, at least 0.001 and at most 86400)", args[i]))
			}
			f.segMs = int64(secs * 1000)
		case "--aes-key":
			keyHex = needValue(args, &i)
		case "--aes-key-file":
			keyFile = needValue(args, &i)
		case "--cenc-key-file":
			cencKeyFile = needValue(args, &i)
		case "--cenc-inline-key":
			cencInline = true
		case "--aes-key-uri":
			keyURI = needValue(args, &i)
		case "--aes-rotate-segments":
			needValue(args, &i)
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 {
				Fatal(fmt.Sprintf("invalid --aes-rotate-segments %q (positive integer)", args[i]))
			}
			rotateSegs = n
		case "--cenc-scheme":
			cencScheme = needValue(args, &i)
		case "--cenc-key":
			cencKeyHex = needValue(args, &i)
		case "--cenc-kid":
			cencKIDHex = needValue(args, &i)
		case "--cenc-iv":
			cencIVHex = needValue(args, &i)
		case "--cenc-key-uri":
			cencKeyURI = needValue(args, &i)
		case "--url-prefix":
			f.prefix = needValue(args, &i)
		case "--single-file":
			f.singleFile = true
		case "--keep-tracks":
			needValue(args, &i)
			for _, p := range strings.Split(args[i], ",") {
				id, err := strconv.ParseUint(strings.TrimSpace(p), 10, 64)
				if err != nil {
					Fatal(fmt.Sprintf("invalid --keep-tracks id %q (comma-separated track IDs)", p))
				}
				f.keepTracks = append(f.keepTracks, id)
			}
		case "--keep-lang", "--audio-lang":
			needValue(args, &i)
			for _, l := range strings.Split(args[i], ",") {
				if l = strings.TrimSpace(l); l != "" {
					f.keepLangs = append(f.keepLangs, strings.ToLower(l))
				}
			}
		case "--sub-offset":
			needValue(args, &i)
			ms, err := strconv.ParseInt(args[i], 10, 64)
			if err != nil {
				Fatal(fmt.Sprintf("invalid --sub-offset %q (integer milliseconds, negative allowed)", args[i]))
			}
			f.subOffset = ms
		case "--chapter-markers":
			f.chapterMarkers = true
		case "--skip-unsupported", "-skip-unsupported":
			f.skipUnsupported = true
		case "--no-iframes":
			f.noIFrames = true
		case "--damage-bytes":
			needValue(args, &i)
			n, err := strconv.ParseInt(args[i], 10, 64)
			if err != nil || n == 0 {
				Fatal(fmt.Sprintf("invalid --damage-bytes %q (a positive byte count, or -1 to refuse any damage)", args[i]))
			}
			f.damageBytes = n
		case "--source-access":
			if i+1 >= len(args) {
				Fatal("--source-access needs auto, local or network")
			}
			i++
			switch args[i] {
			case "auto":
				f.sourceAccess = mp4.SourceAuto
			case "local":
				f.sourceAccess = mp4.SourceLocal
			case "network":
				f.sourceAccess = mp4.SourceNetwork
			default:
				Fatal("--source-access takes auto, local or network, not " + args[i])
			}
		case "--synthesize-index":
			f.synthIndex = true
		case "--audio-shift":
			if i+1 >= len(args) {
				Fatal("--audio-shift needs a <track>=<ms> value")
			}
			i++
			track, ms, err := parseShift(args[i])
			if err != nil {
				Fatal(strings.ReplaceAll(err.Error(), "--shift", "--audio-shift"))
			}
			if f.audioShift == nil {
				f.audioShift = map[uint64]int64{}
			}
			f.audioShift[track] = ms * 1_000_000 // ms -> ns
		default:
			rejectFlagArg(args[i])
			f.rest = append(f.rest, args[i])
		}
	}
	keyHex = keyValue(keyHex, keyFile, "MKVGO_AES_KEY", "--aes-key", keyURI != "")
	cencKeyHex = keyValue(cencKeyHex, cencKeyFile, "MKVGO_CENC_KEY", "--cenc-key", cencScheme != "" || cencKIDHex != "" || cencIVHex != "" || cencKeyURI != "")
	if keyHex != "" || keyURI != "" {
		// --aes-key and --aes-key-uri take a single value, or a comma-separated
		// list when rotating (--aes-rotate-segments N): the keys cycle every N
		// segments, key i served at URI i, so a captured key expires mid-video.
		if rotateSegs > 0 {
			keys := splitCSV(keyHex)
			uris := splitCSV(keyURI)
			if len(keys) < 2 || len(keys) != len(uris) {
				Fatal("--aes-rotate-segments needs matching comma-separated --aes-key and --aes-key-uri lists of at least 2 entries")
			}
			var ks []mp4.HLSKey
			for j := range keys {
				k, err := hex.DecodeString(keys[j])
				if err != nil {
					Fatal(fmt.Sprintf("invalid --aes-key entry %d (expected 32 hex chars): %s", j+1, err.Error()))
				}
				ks = append(ks, mp4.HLSKey{Key: k, KeyURI: uris[j]})
			}
			f.encrypt = &mp4.HLSEncryption{RotateEverySegments: rotateSegs, Keys: ks}
		} else {
			key, err := hex.DecodeString(keyHex)
			if err != nil {
				Fatal("invalid --aes-key (expected 32 hex chars): " + err.Error())
			}
			f.encrypt = &mp4.HLSEncryption{Key: key, KeyURI: keyURI}
		}
	}
	if cencScheme != "" || cencKeyHex != "" || cencKIDHex != "" || cencIVHex != "" {
		key := mustHex(cencKeyHex, "--cenc-key (expected 32 hex chars)")
		kid := mustHex(cencKIDHex, "--cenc-kid (expected 32 hex chars)")
		iv := mustHex(cencIVHex, "--cenc-iv (expected 16 or 32 hex chars)")
		if cencKeyURI == "" && !cencInline {
			Fatal("--cenc-key-uri is required: without it the playlist would carry the content key in clear (a data: URI). Pass --cenc-key-uri with the URI of your key or license server, or --cenc-inline-key to embed the key on purpose (testing only)")
		}
		f.cenc = &mp4.CENCOptions{Scheme: cencScheme, Key: key, KeyID: kid, IV: iv, KeyURI: cencKeyURI, InlineKey: cencInline}
	}
	return f
}

// fatalCapture points stderr at a file, so what a Fatal wrote can be read back after each call.
type fatalCapture struct {
	f   *os.File
	off int64
}

func captureFatals(t *testing.T) *fatalCapture {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	old, oldExit := os.Stderr, osExit
	os.Stderr, osExit = f, func(int) { panic(errExited) }
	t.Cleanup(func() { os.Stderr, osExit = old, oldExit; f.Close() })
	return &fatalCapture{f: f}
}

var errExited = errors.New("exited")

// run calls fn and returns the message a Fatal exited with, "" when none did.
func (c *fatalCapture) run(fn func()) (fatal string) {
	defer func() {
		if r := recover(); r != nil {
			if r != errExited {
				panic(r)
			}
			end, _ := c.f.Seek(0, io.SeekCurrent)
			b := make([]byte, end-c.off)
			_, _ = c.f.ReadAt(b, c.off)
			c.off = end
			fatal = string(b)
		}
	}()
	fn()
	return ""
}

func runParse(c *fatalCapture, parse func([]string) hlsFlags, args []string) (f hlsFlags, fatal string) {
	fatal = c.run(func() { f = parse(args) })
	return f, fatal
}

func TestCLI03_HLSFlagsParity(t *testing.T) {
	values := map[string][]string{
		"-o": {"out"}, "-segment": {"6", "0.5", "x", "-1", "1e300", "NaN", "0.0004"}, "--segment": {"4"},
		"--aes-key":     {"00112233445566778899aabbccddeeff", "zz", "00112233445566778899aabbccddeeff,ffeeddccbbaa99887766554433221100"},
		"--aes-key-uri": {"https://k/a", "https://k/a,https://k/b"}, "--aes-rotate-segments": {"2", "0", "x"},
		"--cenc-scheme": {"cenc", "cbcs"}, "--cenc-key": {"00112233445566778899aabbccddeeff"}, "--cenc-kid": {"ffeeddccbbaa99887766554433221100"},
		"--cenc-iv": {"0102030405060708", "zz"}, "--cenc-key-uri": {"skd://k"}, "--url-prefix": {"https://cdn/"},
		"--keep-tracks": {"1,2", "1, x"}, "--keep-lang": {"fre,ENG", " , "}, "--audio-lang": {"spa"}, "--sub-offset": {"-500", "x"},
		"--damage-bytes": {"100", "0", "-1"}, "--source-access": {"auto", "local", "network", "nfs"}, "--audio-shift": {"2=500", "bad"},
	}
	bools := []string{"--single-file", "--chapter-markers", "--skip-unsupported", "-skip-unsupported", "--no-iframes", "--synthesize-index", "--cenc-inline-key"}
	operands := []string{"in.mkv", "master.m3u8", "-", "--bogus", "-x"}
	var flags []string
	for k := range values {
		flags = append(flags, k)
	}
	c := captureFatals(t)
	rng := rand.New(rand.NewSource(3))
	fatals := 0
	for n := 0; n < 20000; n++ {
		var args []string
		for k := rng.Intn(6); k >= 0; k-- {
			switch rng.Intn(4) {
			case 0:
				args = append(args, operands[rng.Intn(len(operands))])
			case 1:
				args = append(args, bools[rng.Intn(len(bools))])
			default:
				fl := flags[rng.Intn(len(flags))]
				args = append(args, fl)
				if rng.Intn(8) > 0 {
					vs := values[fl]
					args = append(args, vs[rng.Intn(len(vs))])
				}
			}
		}
		got, gf := runParse(c, parseHLSFlags, args)
		want, wf := runParse(c, oldParseHLSFlags, args)
		if gf != wf || (gf == "" && !reflect.DeepEqual(got, want)) {
			t.Fatalf("%q:\nnew %+v %q\nold %+v %q", args, got, gf, want, wf)
		}
		if gf != "" {
			fatals++
		}
	}
	if fatals < 1000 || fatals > 19000 {
		t.Fatalf("%d of the vectors exit: the corpus does not test both sides", fatals)
	}
}
