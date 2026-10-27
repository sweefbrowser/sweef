module github.com/sweefbrowser/sweef

go 1.26.9

require (
	github.com/dsnet/compress v0.0.1
	github.com/ebitengine/oto/v3 v3.5.1
	github.com/ebitengine/purego v0.11.1
	github.com/hajimehoshi/go-mp3 v0.3.4
	github.com/jfreymuth/oggvorbis v1.0.5
	github.com/jfreymuth/vorbis v1.0.2
	github.com/pion/ice/v4 v4.4.7
	github.com/pion/interceptor v0.1.49
	github.com/pion/logging v0.2.4
	github.com/pion/rtcp v1.2.19
	github.com/pion/rtp v1.10.5
	github.com/pion/sdp/v3 v3.0.20
	github.com/pion/transport/v5 v5.1.1
	github.com/pion/webrtc/v4 v4.2.23
	github.com/syndtr/goleveldb v1.0.1-0.20220721030215-126854af5e6d
	github.com/tetratelabs/wazero v1.12.0
	github.com/thesyncim/gopus v0.1.2
	golang.org/x/image v0.47.0
	golang.org/x/net v0.61.0
	golang.org/x/sys v0.49.0
	golang.org/x/text v0.43.0
	modernc.org/libc v1.79.0
	modernc.org/libquickjs v0.14.0
	modernc.org/quickjs v0.26.0
)

require (
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/golang/snappy v0.0.4 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jfreymuth/pulse v0.1.3 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/pion/datachannel v1.6.3 // indirect
	github.com/pion/dtls/v3 v3.1.10 // indirect
	github.com/pion/mdns/v2 v2.2.2 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/sctp v1.12.0 // indirect
	github.com/pion/srtp/v3 v3.1.3 // indirect
	github.com/pion/stun/v4 v4.0.1 // indirect
	github.com/pion/turn/v5 v5.1.2 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

replace github.com/thesyncim/gopus => ./third_party/gopus

replace github.com/hajimehoshi/go-mp3 => ./third_party/go-mp3
