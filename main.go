package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

type ocsResponse struct {
	OCS struct {
		Meta struct {
			StatusCode int    `json:"statuscode"`
			Message    string `json:"message"`
		} `json:"meta"`
		Data json.RawMessage `json:"data"`
	} `json:"ocs"`
}

type talkSettings struct {
	Server          string                     `json:"server"`
	HelloAuthParams map[string]json.RawMessage `json:"helloAuthParams"`
}

type signalingMessage struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Hello struct {
		SessionID string `json:"sessionid"`
	} `json:"hello"`
	Room struct {
		RoomID string `json:"roomid"`
	} `json:"room"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Message struct {
		Data struct {
			Type    string `json:"type"`
			SID     string `json:"sid"`
			Payload struct {
				Type      string          `json:"type"`
				SDP       string          `json:"sdp"`
				Candidate json.RawMessage `json:"candidate"`
			} `json:"payload"`
		} `json:"data"`
	} `json:"message"`
}

type safeSocket struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (s *safeSocket) send(v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.WriteJSON(v)
}

func main() {
	ncArg := flag.String("nc", "", "Nextcloud HTTPS hostname or URL")
	talkID := flag.String("talkid", "", "Talk meeting token from /call/<token>")
	duration := flag.Duration("duration", 60*time.Second, "Media transmission duration per tester after WebRTC connects")
	testers := flag.Int("testers", 1, "Number of concurrent independent guest testers")
	mediaDir := flag.String("media-dir", "", "Directory containing test-audio.ogg and test-video.ivf (empty: Opus silence only)")
	ramp := flag.Duration("ramp", 2*time.Second, "Delay between launching guest testers")
	debug := flag.Bool("debug", false, "Enable verbose per-tester, ICE, signaling and RTP diagnostics")
	flag.Parse()
	if *ncArg == "" || *talkID == "" || *duration <= 0 || *testers < 1 || *testers > 500 || *ramp < 0 {
		flag.Usage()
		os.Exit(2)
	}
	base := strings.TrimRight(*ncArg, "/")
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" {
		log.Fatal("-nc must be a valid HTTPS hostname or URL")
	}
	if strings.ContainsAny(*talkID, "/?#") {
		log.Fatal("-talkid must contain only the meeting token")
	}
	base = parsed.String()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("Starting Nextcloud Talk native test: testers=%d, duration=%s, ramp=%s", *testers, *duration, *ramp)
	type result struct {
		id  int
		err error
	}
	results := make(chan result, *testers)
	started := 0
launchLoop:
	for i := 1; i <= *testers; i++ {
		if i > 1 && *ramp > 0 {
			select {
			case <-ctx.Done():
				break launchLoop
			case <-time.After(*ramp):
			}
		}
		if ctx.Err() != nil {
			break
		}
		started++
		go func(id int) {
			flags := 3 // In-call + audio
			if *mediaDir != "" {
				flags = 7 // In-call + audio + video
			}
			results <- result{id, run(ctx, base, *talkID, *duration, flags, id, *mediaDir, *debug)}
		}(i)
	}
	passed, failed := 0, 0
	for i := 0; i < started; i++ {
		r := <-results
		if r.err == nil {
			passed++
			log.Printf("OK Tester %03d (transport connected; receiver media not validated)", r.id)
		} else {
			failed++
			log.Printf("FAIL Tester %d: %v", r.id, r.err)
		}
	}
	log.Printf("Summary: started=%d, connected=%d, failed=%d (receiver media not validated)", started, passed, failed)
	if failed > 0 || started != *testers {
		os.Exit(1)
	}
}

func run(ctx context.Context, base, token string, duration time.Duration, callFlags, testerID int, mediaDir string, debug bool) error {
	// Keep detailed logs silent unless -debug is supplied. Fatal errors and the
	// final per-tester result are reported by main in either mode.
	output := io.Writer(io.Discard)
	if debug {
		output = os.Stdout
	}
	logger := log.New(output, fmt.Sprintf("[Tester %03d] ", testerID), log.LstdFlags)
	var audioFile, videoFile *os.File
	var audioReader *oggreader.OggReader
	var videoReader *ivfreader.IVFReader
	var videoFrameDuration = 40 * time.Millisecond
	if mediaDir != "" {
		var err error
		audioFile, err = os.Open(filepath.Join(mediaDir, "test-audio.ogg"))
		if err != nil {
			return fmt.Errorf("open audio: %w", err)
		}
		defer audioFile.Close()
		audioReader, _, err = oggreader.NewWith(audioFile)
		if err != nil {
			return fmt.Errorf("read Ogg: %w", err)
		}
		videoFile, err = os.Open(filepath.Join(mediaDir, "test-video.ivf"))
		if err != nil {
			return fmt.Errorf("open video: %w", err)
		}
		defer videoFile.Close()
		var header *ivfreader.IVFFileHeader
		videoReader, header, err = ivfreader.NewWith(videoFile)
		if err != nil {
			return fmt.Errorf("read IVF: %w", err)
		}
		if header.FourCC != "VP80" {
			return fmt.Errorf("expected VP8 IVF, got %q", header.FourCC)
		}
		if header.TimebaseNumerator == 0 || header.TimebaseDenominator == 0 {
			return errors.New("invalid IVF timebase")
		}
		videoFrameDuration = time.Duration(float64(time.Second) * float64(header.TimebaseNumerator) / float64(header.TimebaseDenominator))
		if videoFrameDuration <= 0 {
			return errors.New("invalid IVF frame duration")
		}
		logger.Printf("Media: Ogg Opus + IVF VP8 (%s per video frame)", videoFrameDuration)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	httpClient := &http.Client{Jar: jar, Timeout: 20 * time.Second}
	root := base + "/ocs/v2.php/apps/spreed/api/"
	roomPath := root + "v4/room/" + url.PathEscape(token)
	callPath := root + "v4/call/" + url.PathEscape(token)
	callJoined := false
	defer func() {
		if !callJoined {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := ocsRequest(cleanupCtx, httpClient, http.MethodDelete, callPath, nil); err != nil {
			log.Printf("WARNING Tester %03d: failed to leave Nextcloud call: %v", testerID, err)
		} else {
			logger.Print("Left Nextcloud call")
		}
	}()

	// Each tester has its own isolated HTTP cookie jar and HPB / WebRTC session.
	if _, err := ocsRequest(ctx, httpClient, http.MethodPost, roomPath+"/participants/active", nil); err != nil {
		return fmt.Errorf("guest activation: %w", err)
	}
	logger.Print("Nextcloud guest session activated")

	// Set a unique guest display name using the official Talk guest API.
	// This must use the same cookie jar as the guest activation request.
	guestName := fmt.Sprintf("Tester %03d", testerID)
	namePath := root + "v1/guest/" + url.PathEscape(token) + "/name"
	if _, err := ocsRequest(ctx, httpClient, http.MethodPost, namePath,
		url.Values{"displayName": []string{guestName}}); err != nil {
		return fmt.Errorf("set guest display name %q: %w", guestName, err)
	}
	logger.Printf("Guest display name set to %q", guestName)

	roomData, err := ocsRequest(ctx, httpClient, http.MethodGet, roomPath, nil)
	if err != nil {
		return fmt.Errorf("room details: %w", err)
	}
	var room struct {
		SessionID string `json:"sessionId"`
		Token     string `json:"token"`
	}
	if err := json.Unmarshal(roomData, &room); err != nil {
		return err
	}
	if room.SessionID == "" || room.SessionID == "0" || room.Token != token {
		return errors.New("no valid Nextcloud room session")
	}
	logger.Printf("Nextcloud room session ID: %s", room.SessionID)

	settingsURL := root + "v3/signaling/settings?token=" + url.QueryEscape(token)
	settingsData, err := ocsRequest(ctx, httpClient, http.MethodGet, settingsURL, nil)
	if err != nil {
		return fmt.Errorf("signaling settings: %w", err)
	}
	var settings talkSettings
	if err := json.Unmarshal(settingsData, &settings); err != nil {
		return err
	}
	auth := settings.HelloAuthParams["2.0"]
	if len(auth) == 0 {
		return errors.New("helloAuthParams 2.0 missing")
	}
	var authCheck struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(auth, &authCheck); err != nil || authCheck.Token == "" {
		return errors.New("hello-v2 JWT missing")
	}
	if settings.Server == "" {
		return errors.New("HPB server missing from settings")
	}

	// Intentionally use direct ICE candidates, matching the validated test
	// implementation. Server-provided STUN/TURN is not applied here.
	pcConfig := webrtc.Configuration{}

	counters := &rtpCounters{}

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return err
	}

	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, registry); err != nil {
		return err
	}

	registry.Add(&rtpCounterFactory{
		counters: counters,
		logger:   logger,
		debug:    debug,
	})

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(registry),
	)

	pc, err := api.NewPeerConnection(pcConfig)

	if err != nil {
		return err
	}
	defer pc.Close()
	logger.Printf("ICE servers configured: %d", len(pcConfig.ICEServers))
	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) { logger.Printf("ICE: %s", st.String()) })
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "native-talk")
	if err != nil {
		return err
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		return err
	}
	var videoTrack *webrtc.TrackLocalStaticSample
	if mediaDir != "" {
		videoTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "video", "native-talk")
		if err != nil {
			return err
		}
		videoSender, err := pc.AddTrack(videoTrack)
		if err != nil {
			return err
		}
		go func() {
			buf := make([]byte, 1500)
			for {
				if _, _, err := videoSender.Read(buf); err != nil {
					return
				}
			}
		}()
	}
	// Read RTCP feedback so the sender does not accumulate unprocessed control messages.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()

	// Non-trickle local ICE: gather all local candidates into the initial offer.
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(25 * time.Second):
		return errors.New("local ICE gathering timeout")
	}
	logger.Printf("Pion SDP offer ready (%d bytes)", len(pc.LocalDescription().SDP))

	wsURL := strings.TrimSuffix(settings.Server, "/") + "/spreed"
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("HPB websocket: %w", err)
	}
	socket := &safeSocket{conn: conn}
	defer conn.Close()
	go func() { <-ctx.Done(); _ = conn.Close() }()
	if err := socket.send(map[string]any{"id": "auth-1", "type": "hello", "hello": map[string]any{"version": "2.0", "auth": map[string]any{"url": base, "params": json.RawMessage(auth)}}}); err != nil {
		return err
	}

	var ownHPBSession string
	var offerSent, answerReceived bool
	candidateQueue := []webrtc.ICECandidateInit{}
	connected := make(chan struct{}, 1)
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		logger.Printf("PeerConnection: %s", st.String())
		if st == webrtc.PeerConnectionStateConnected {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})

	// Read WebSocket frames in one goroutine; never poll a WebSocket after a read timeout.
	type frameResult struct {
		data []byte
		err  error
	}
	frames := make(chan frameResult, 16)
	go func() {
		for {
			_, b, err := conn.ReadMessage()
			select {
			case frames <- frameResult{b, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	callStart := make(chan error, 1)
	startupTimer := time.NewTimer(60 * time.Second)
	defer startupTimer.Stop()
	var audioTicker *time.Ticker
	var videoTicker *time.Ticker
	videoFrames := 0
	var durationTimer *time.Timer
	packets := 0
	defer func() {
		if audioTicker != nil {
			audioTicker.Stop()
		}
		if videoTicker != nil {
			videoTicker.Stop()
		}
		if durationTimer != nil {
			durationTimer.Stop()
		}
	}()

	for {
		var tick <-chan time.Time
		var videoTick <-chan time.Time
		if videoTicker != nil {
			videoTick = videoTicker.C
		}
		var testEnd <-chan time.Time
		if audioTicker != nil {
			tick = audioTicker.C
		}
		if durationTimer != nil {
			testEnd = durationTimer.C
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-startupTimer.C:
			return errors.New("HPB media setup or ICE connection timeout")
		case <-testEnd:
			logger.Printf("Test finished: %d audio frames, %d video frames submitted to Pion", packets, videoFrames)
			if debug {
				counters.print(logger)
				logRTPStats(pc, logger)
			}
			return nil
		case <-tick:
			if pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
				return errors.New("WebRTC connection lost")
			}
			audioPayload := []byte{0xF8, 0xFF, 0xFE}
			if audioReader != nil {
				var readErr error
				audioPayload, _, readErr = audioReader.ParseNextPage()
				if errors.Is(readErr, io.EOF) {
					if _, readErr = audioFile.Seek(0, io.SeekStart); readErr != nil {
						return readErr
					}
					audioReader, _, readErr = oggreader.NewWith(audioFile)
					if readErr == nil {
						audioPayload, _, readErr = audioReader.ParseNextPage()
					}
				}
				if readErr != nil {
					return fmt.Errorf("audio read: %w", readErr)
				}
				if len(audioPayload) == 0 {
					continue
				}
			}
			if err := track.WriteSample(media.Sample{Data: audioPayload, Duration: 20 * time.Millisecond}); err != nil {
				return err
			}
			packets++
		case <-videoTick:
			if pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
				return errors.New("WebRTC connection lost")
			}
			payload, _, readErr := videoReader.ParseNextFrame()
			if errors.Is(readErr, io.EOF) {
				if _, readErr = videoFile.Seek(0, io.SeekStart); readErr != nil {
					return readErr
				}
				videoReader, _, readErr = ivfreader.NewWith(videoFile)
				if readErr == nil {
					payload, _, readErr = videoReader.ParseNextFrame()
				}
			}
			if readErr != nil {
				return fmt.Errorf("video read: %w", readErr)
			}
			if err := videoTrack.WriteSample(media.Sample{Data: payload, Duration: videoFrameDuration}); err != nil {
				return err
			}
			videoFrames++
		case <-connected:
			if audioTicker == nil {
				logger.Print("WebRTC connected; starting media")
				startupTimer.Stop()
				audioTicker = time.NewTicker(20 * time.Millisecond)
				if videoTrack != nil {
					videoTicker = time.NewTicker(videoFrameDuration)
				}
				durationTimer = time.NewTimer(duration)
			}
		case err := <-callStart:
			if err != nil {
				return fmt.Errorf("call join: %w", err)
			}
			callJoined = true
			logger.Print("Talk call joined successfully")
			offerSent = true
			if err := socket.send(map[string]any{
				"id": "media-1", "type": "message",
				"message": map[string]any{
					"recipient": map[string]any{"type": "session", "sessionid": ownHPBSession},
					"data": map[string]any{
						"to": ownHPBSession, "type": "offer", "sid": "native-audio-1", "roomType": "video",
						"payload":    map[string]any{"type": "offer", "sdp": pc.LocalDescription().SDP, "nick": "Native Talk Test"},
						"audiocodec": "opus",
					},
				},
			}); err != nil {
				return err
			}
			logger.Print("Media SDP offer sent through HPB")
		case f := <-frames:
			if f.err != nil {
				return fmt.Errorf("HPB WebSocket: %w", f.err)
			}
			var msg signalingMessage
			if err := json.Unmarshal(f.data, &msg); err != nil {
				logger.Printf("Ignoring invalid HPB JSON: %v", err)
				continue
			}
			if msg.Type == "event" {
				logger.Print("HPB event received")
			}
			if msg.Type == "error" {
				return fmt.Errorf("HPB error [%s]: %s (%s)", msg.ID, msg.Error.Code, msg.Error.Message)
			}
			switch {
			case msg.ID == "auth-1" && msg.Type == "hello":
				ownHPBSession = msg.Hello.SessionID
				if ownHPBSession == "" {
					return errors.New("HPB session ID missing")
				}
				logger.Print("HPB authentication successful")
				logger.Printf("HPB session ID: %s", ownHPBSession)
				if err := socket.send(map[string]any{"id": "room-1", "type": "room", "room": map[string]any{"roomid": token, "sessionid": room.SessionID}}); err != nil {
					return err
				}
			case msg.ID == "room-1" && msg.Type == "room":
				if msg.Room.RoomID != token {
					return errors.New("room ID mismatch")
				}
				logger.Print("HPB room join successful")
				go func() {
					form := url.Values{"flags": []string{fmt.Sprint(callFlags)}}
					_, err := ocsRequest(ctx, httpClient, http.MethodPost, callPath, form)
					callStart <- err
				}()
			case msg.Type == "message" && msg.Message.Data.Type == "answer":
				data := msg.Message.Data
				if !offerSent || data.SID != "native-audio-1" || data.Payload.Type != "answer" || data.Payload.SDP == "" {
					continue
				}
				if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: data.Payload.SDP}); err != nil {
					return fmt.Errorf("SetRemoteDescription: %w", err)
				}
				answerReceived = true
				logger.Print("Janus SDP answer applied")
				for _, line := range strings.Split(data.Payload.SDP, "\n") {
					line = strings.TrimSpace(line)

					if strings.HasPrefix(line, "m=audio ") ||
						strings.HasPrefix(line, "m=video ") ||
						strings.HasPrefix(line, "a=mid:") ||
						strings.HasPrefix(line, "a=inactive") ||
						strings.HasPrefix(line, "a=recvonly") ||
						strings.HasPrefix(line, "a=sendrecv") {
						logger.Printf("ANSWER SDP: %s", line)
					}
				}
				for _, c := range candidateQueue {
					if err := pc.AddICECandidate(c); err != nil {
						return fmt.Errorf("ICE candidate rejected: %w", err)
					}
				}
				candidateQueue = nil
			case msg.Type == "message" && msg.Message.Data.Type == "candidate":
				data := msg.Message.Data
				if data.SID != "native-audio-1" {
					continue
				}
				var payload struct {
					Candidate     string  `json:"candidate"`
					SDPMid        *string `json:"sdpMid"`
					SDPMLineIndex *uint16 `json:"sdpMLineIndex"`
				}
				if json.Unmarshal(data.Payload.Candidate, &payload) != nil || payload.Candidate == "" {
					continue
				}
				c := webrtc.ICECandidateInit{Candidate: payload.Candidate, SDPMid: payload.SDPMid, SDPMLineIndex: payload.SDPMLineIndex}
				if !answerReceived {
					candidateQueue = append(candidateQueue, c)
				} else if err := pc.AddICECandidate(c); err != nil {
					return fmt.Errorf("ICE candidate rejected: %w", err)
				}
			}
		}
	}
}

func ocsRequest(ctx context.Context, cli *http.Client, method, endpoint string, form url.Values) (json.RawMessage, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("OCS-APIRequest", "true")
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d op %s", resp.StatusCode, req.URL.Path)
	}
	if !json.Valid(bytes.TrimSpace(data)) {
		return nil, fmt.Errorf("non-JSON response from %s", req.URL.Path)
	}
	var decoded ocsResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	if decoded.OCS.Meta.StatusCode != 200 {
		return nil, fmt.Errorf("OCS %d op %s: %s", decoded.OCS.Meta.StatusCode, req.URL.Path, decoded.OCS.Meta.Message)
	}
	return decoded.OCS.Data, nil
}

func logRTPStats(pc *webrtc.PeerConnection, logger *log.Logger) {
	stats := pc.GetStats()

	logger.Printf("Total WebRTC stats reports: %d", len(stats))

	for id, report := range stats {
		logger.Printf("Stats: id=%s, type=%T", id, report)
	}
}
