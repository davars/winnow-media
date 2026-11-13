package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
)

// videoFrameDelayCS is how long each sample is displayed in an animated
// output, in centiseconds: two frames a second.
const videoFrameDelayCS = 50

// videoRequest is what the sampling of a video needs.
type videoRequest struct {
	// times chooses the sample times, in ms, from the probe: ascending
	// without repeats. It is nil when run is set.
	times func(videoInfo) ([]int64, error)
	// run, when set, samples consecutive frames instead of times.
	run *frameRun
}

// frameRun is count consecutive frames from the first at or after startMS.
type frameRun struct {
	startMS int64
	count   int
}

// videoSource is what a frameSource holds of a video: the probe and the
// samples, which the encode decodes.
type videoSource struct {
	dec  decoder
	path string
	info videoInfo
	run  *frameRun
	// times are the sample times asked for until the samples are decoded, then
	// the time of each sample.
	times []int64
	// plan is how the decoded samples are sized; set once decoded.
	plan    videoPlan
	decoded bool
}

// videoFromProbe makes the source of the samples the request names of a video
// with the given probe. It decodes nothing, since the size to decode at depends
// on the encodings of the encode.
func (e toolEnv) videoFromProbe(req openRequest, info videoInfo) (*frameSource, error) {
	v := &videoSource{dec: req.dec, path: req.path, info: info, run: req.video.run}
	var err error
	if v.run != nil {
		err = checkSeekTime(info, v.run.startMS)
	} else {
		v.times, err = req.video.times(info)
	}
	if err != nil {
		return nil, err
	}
	s := &frameSource{env: e, route: routeVideo, video: v, dir: req.scratch}
	s.layout.uw, s.layout.uh = info.upright()
	s.layout.cw, s.layout.ch = info.width, info.height
	s.upright = [2]int{s.layout.uw, s.layout.uh}
	s.setSamples(len(v.times))
	return s, nil
}

// setSamples makes the source hold n samples, numbered from 0.
func (s *frameSource) setSamples(n int) {
	s.selected = make([]int, n)
	s.layout.infos = make([]frameInfo, n)
	for i := range n {
		s.selected[i] = i
		s.layout.infos[i] = frameInfo{delayCS: videoFrameDelayCS}
	}
}

// videoPlanFor plans the sampling of the region r at the size of the largest
// of encs.
func (s *frameSource) videoPlanFor(r region, encs []encoding) videoPlan {
	v := s.video
	w, h := v.info.upright()
	decode, best := encs[0], 0
	for _, enc := range encs {
		sw, sh := r.subject(w, h)
		tw, th, _ := fitSize(sw, sh, enc.maxEdge, enc.maxArea)
		if tw*th > best {
			decode, best = enc, tw*th
		}
	}
	return newVideoPlan(v.info, r, decode, s.env.threads())
}

// decodeVideo samples the video at the size the encodes of encs need, unless
// the samples already decoded serve them. A source decoded for a smaller
// encoding is decoded again. A failed decode leaves no samples.
func (s *frameSource) decodeVideo(ctx context.Context, r region, encs []encoding) error {
	v := s.video
	plan := s.videoPlanFor(r, encs)
	// One time is cheaper by a seek than by decoding from the start.
	plan.selectFrames = plan.selectFrames && (v.run != nil || len(v.times) > 1)
	if v.decoded && v.plan.covers(plan) {
		return nil
	}
	removeFiles(s.framePaths())
	s.frames, v.decoded = nil, false
	var samples []sampledFrame
	var err error
	if v.run != nil {
		samples, err = s.env.runFrames(ctx, v.dec, v.path, s.dir, plan, v.run.startMS, v.run.count, !plan.selectFrames)
		if err != nil {
			err = fmt.Errorf("sampling frames from %d ms: %w", v.run.startMS, err)
		}
	} else {
		samples, err = s.env.sampleVideo(ctx, v.dec, v.path, s.dir, plan, v.times)
		// The times are guesses from the duration, which can lie past the
		// only frame of a clip whose frame count is unknown.
		if err == nil && len(samples) == 0 && !slices.Equal(v.times, []int64{0}) {
			samples, err = s.env.sampleVideo(ctx, v.dec, v.path, s.dir, plan, []int64{0})
		}
	}
	if err == nil && len(samples) == 0 {
		err = errors.New("no frame could be sampled")
	}
	if err != nil {
		for _, sm := range samples {
			_ = os.Remove(sm.path)
		}
		return err
	}
	v.plan, v.decoded = plan, true
	v.times = make([]int64, len(samples))
	for i, sm := range samples {
		s.frames = append(s.frames, srcFrame{path: sm.path, sourceFrame: i})
		v.times[i] = sm.timeMS
	}
	s.setSamples(len(samples))
	return nil
}
