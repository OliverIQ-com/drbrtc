package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Global port counter with mutex for thread safety
var (
	nextPort = 5004
	portMux  sync.Mutex
)

func findAvailablePort(startPort int) int {
	// Try ports until we find an available one
	port := startPort
	for {
		addr := fmt.Sprintf(":%d", port)
		conn, err := net.ListenPacket("udp", addr)
		if err == nil {
			conn.Close()
			return port
		}

		log.Printf("Port %d is unavailable, trying port %d", port, port+1)
		port++

		// Avoid excessive looping
		if port > 65000 {
			port = 5004 // Reset to start if we've gone too high
		}
	}
}

// getNextPort returns a unique port number for each connection
func getNextPort() int {
	portMux.Lock()
	defer portMux.Unlock()

	availablePort := findAvailablePort(nextPort)
	nextPort = availablePort + 1
	return availablePort
}

func main() {
	// Setup media engine
	m := webrtc.MediaEngine{}
	err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeH264,
			ClockRate:    90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: nil,
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo)
	if err != nil {
		log.Fatal("Failed to register codec:", err)
	}

	// Setup ICE settings
	settingEngine := webrtc.SettingEngine{}

	// Optional: Configure port range for WebRTC/ICE (separate from our ffmpeg ports)
	// This restricts what ports ICE will use for media
	settingEngine.SetEphemeralUDPPortRange(10000, 20000)

	// Create the API with our settings
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(&m),
		webrtc.WithSettingEngine(settingEngine),
	)

	http.HandleFunc("/offer", func(w http.ResponseWriter, r *http.Request) {
		log.Println("[/offer] Received SDP offer")

		// Allow CORS so browser clients can reach us
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// Optional: preflight support if you add complex headers/methods
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		offerSDP := r.FormValue("sdp")
		if offerSDP == "" {
			http.Error(w, "missing SDP", http.StatusBadRequest)
			log.Println("[/offer] Missing SDP in POST body")
			return
		}

		log.Println("[/offer] SDP offer length:", len(offerSDP))

		// ICE servers configuration
		webrtcConfig := webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{
				{
					URLs: []string{"stun:stun.l.google.com:19302"},
				},
				/*
					// For production, replace with your own TURN server credentials
					// Example for a service like Twilio or your custom TURN server
					{
						URLs:       []string{"turn:global.turn.twilio.com:3478?transport=udp"},
						Username:   "your_username_here",        // Replace with actual credentials
						Credential: "your_credential_here",      // Replace with actual credentials
					},
				*/
			},
		}

		peerConnection, err := api.NewPeerConnection(webrtcConfig)
		if err != nil {
			log.Println("[/offer] Error creating PeerConnection:", err)
			http.Error(w, "failed to create PeerConnection", http.StatusInternalServerError)
			return
		}

		// Add connection state callback
		peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
			log.Printf("[PeerConnection] State changed to %s\n", state.String())

			if state == webrtc.PeerConnectionStateClosed ||
				state == webrtc.PeerConnectionStateFailed ||
				state == webrtc.PeerConnectionStateDisconnected {
				log.Println("[PeerConnection] Cleaning up connection")
				if err := peerConnection.Close(); err != nil {
					log.Println("[PeerConnection] Error closing connection:", err)
				}
			}
		})

		videoTrack, err := webrtc.NewTrackLocalStaticRTP(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
			"video", "pion",
		)
		if err != nil {
			log.Println("[/offer] Error creating video track:", err)
			http.Error(w, "failed to create track", http.StatusInternalServerError)
			return
		}

		_, err = peerConnection.AddTrack(videoTrack)
		if err != nil {
			log.Println("[/offer] Error adding video track:", err)
			http.Error(w, "failed to add track", http.StatusInternalServerError)
			return
		}

		offer := webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  offerSDP,
		}

		if err := peerConnection.SetRemoteDescription(offer); err != nil {
			log.Println("[/offer] Error setting remote description:", err)
			http.Error(w, "invalid SDP", http.StatusBadRequest)
			return
		}
		log.Println("[/offer] Remote description set successfully")

		answer, err := peerConnection.CreateAnswer(nil)
		if err != nil {
			log.Println("[/offer] Error creating answer:", err)
			http.Error(w, "failed to create answer", http.StatusInternalServerError)
			return
		}

		if err := peerConnection.SetLocalDescription(answer); err != nil {
			log.Println("[/offer] Error setting local description:", err)
			http.Error(w, "failed to set local description", http.StatusInternalServerError)
			return
		}

		// Get a unique port for this connection
		port := getNextPort()
		log.Printf("[/offer] Starting ffmpeg and RTP relay on port %d\n", port)
		go startFFmpeg(videoTrack, port)

		<-webrtc.GatheringCompletePromise(peerConnection)

		log.Println("[/offer] ICE gathering complete, returning answer SDP")
		w.Write([]byte(peerConnection.LocalDescription().SDP))
	})

	log.Println("Server listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func startFFmpeg(videoTrack *webrtc.TrackLocalStaticRTP, port int) {
	addr := fmt.Sprintf(":%d", port)
	log.Printf("[ffmpeg:%d] Starting ffmpeg with RTP output to %s", port, addr)

	cmd := exec.Command("ffmpeg",
		"-re", "-stream_loop", "-1", "-i", "test.mp4",
		"-an", "-c:v", "libx264", "-f", "rtp", fmt.Sprintf("rtp://127.0.0.1:%d", port),
	)

	stderr, _ := cmd.StderrPipe()
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("[ffmpeg:%d] %s", port, scanner.Text())
		}
	}()

	if err := cmd.Start(); err != nil {
		log.Printf("[ffmpeg:%d] Failed to start: %v", port, err)
		return
	}

	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Printf("[ffmpeg:%d] Failed to listen for RTP: %v", port, err)
		return
	}
	defer conn.Close()

	log.Printf("[ffmpeg:%d] Listening for RTP packets on %s", port, addr)

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			log.Printf("[ffmpeg:%d] RTP read error: %v", port, err)
			break
		}

		packet := &rtp.Packet{}
		if err := packet.Unmarshal(buf[:n]); err != nil {
			log.Printf("[ffmpeg:%d] RTP unmarshal error: %v", port, err)
			continue
		}

		if err := videoTrack.WriteRTP(packet); err != nil {
			log.Printf("[ffmpeg:%d] RTP write error: %v", port, err)
			break
		}
	}
}
