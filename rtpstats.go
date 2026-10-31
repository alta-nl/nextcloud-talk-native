package main

import (
	"log"
	"strings"
	"sync/atomic"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

type rtpCounters struct {
	audioPackets atomic.Uint64
	audioBytes   atomic.Uint64
	videoPackets atomic.Uint64
	videoBytes   atomic.Uint64
}

func (c *rtpCounters) print(logger *log.Logger) {
	logger.Printf(
		"RTP sent: audio=%d packets/%d bytes, video=%d packets/%d bytes",
		c.audioPackets.Load(),
		c.audioBytes.Load(),
		c.videoPackets.Load(),
		c.videoBytes.Load(),
	)
}

type rtpCounterFactory struct {
	counters *rtpCounters
	logger   *log.Logger
	debug    bool
}

func (f *rtpCounterFactory) NewInterceptor(
	_ string,
) (interceptor.Interceptor, error) {
	return &rtpCounterInterceptor{
		counters: f.counters,
		logger:   f.logger,
		debug:    f.debug,
	}, nil
}

type rtpCounterInterceptor struct {
	interceptor.NoOp

	counters *rtpCounters
	logger   *log.Logger
	debug    bool
}

func (i *rtpCounterInterceptor) BindLocalStream(
	info *interceptor.StreamInfo,
	writer interceptor.RTPWriter,
) interceptor.RTPWriter {
	mime := strings.ToLower(info.MimeType)

	if i.debug {
		i.logger.Printf(
			"RTP stream bound: codec=%s, SSRC=%d, PT=%d",
			info.MimeType,
			info.SSRC,
			info.PayloadType,
		)
	}

	return interceptor.RTPWriterFunc(func(
		header *rtp.Header,
		payload []byte,
		attributes interceptor.Attributes,
	) (int, error) {
		n, err := writer.Write(header, payload, attributes)

		if err == nil {
			switch {
			case strings.HasPrefix(mime, "audio/"):
				i.counters.audioPackets.Add(1)
				i.counters.audioBytes.Add(uint64(n))

			case strings.HasPrefix(mime, "video/"):
				i.counters.videoPackets.Add(1)
				i.counters.videoBytes.Add(uint64(n))
			}
		}

		return n, err
	})
}
