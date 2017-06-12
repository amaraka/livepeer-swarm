//mediaserver is the place we set up the handlers for network requests.

package mediaserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	ethCommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/swarm/network/kademlia"
	"github.com/livepeer/go-livepeer/livepeer/network"
	"github.com/livepeer/go-livepeer/livepeer/storage"
	"github.com/livepeer/go-livepeer/livepeer/streaming"
	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/av/avutil"

	"net/url"

	"github.com/livepeer/lpms"
	lpmsStream "github.com/livepeer/lpms/stream"
	streamingVizClient "github.com/livepeer/streamingviz/client"
	"github.com/nareix/joy4/av/pubsub"
)

var ErrNotFound = errors.New("NotFound")
var ErrStreamPublish = errors.New("StreamPublishError")
var HLSWaitTime = time.Second * 10
var HLSBufferCap = uint(43200) //12 hrs assuming 1s segment
var HLSBufferWindow = uint(5)
var HLSUnsubscribeWaitLimit = time.Second * 20

func startHlsUnsubscribeWorker(hlsSubTimer map[streaming.StreamID]time.Time, streamer *streaming.Streamer, forwarder storage.CloudStore, limit time.Duration) {
	for {
		time.Sleep(time.Second * 5)
		for sid, t := range hlsSubTimer {
			if time.Since(t) > limit {
				streamer.UnsubscribeToHLSStream(sid.String(), "local")
				forwarder.StopStream(sid.String(), kademlia.Address(ethCommon.HexToHash("")), lpmsStream.HLS) //This could fail if it's a local stream, but it's ok.
				delete(hlsSubTimer, sid)
			}
		}
	}
}

func StartLPMS(rtmpPort string, httpPort string, streamer *streaming.Streamer, forwarder storage.CloudStore, streamdb *network.StreamDB,
	viz *streamingVizClient.Client, hive *network.Hive, ffmpegPath string, vodPath string) {

	hlsSubTimer := make(map[streaming.StreamID]time.Time)
	go startHlsUnsubscribeWorker(hlsSubTimer, streamer, forwarder, HLSUnsubscribeWaitLimit)

	server := lpms.New(rtmpPort, httpPort, ffmpegPath, vodPath)

	server.HandleHLSPlay(
		func(reqPath string) (*lpmsStream.HLSBuffer, error) {
			strmID := parseStreamID(reqPath)
			// var strmID string
			// regex, _ := regexp.Compile("\\/stream\\/([[:alpha:]]|\\d)*")
			// match := regex.FindString(reqPath)
			// if match != "" {
			// 	strmID = strings.Replace(match, "/stream/", "", -1)
			// }

			//Validate the stream ID format
			sid := streaming.StreamID(strmID)
			nodeID, streamID := sid.SplitComponents()

			if strmID == "" || streamID == "" {
				glog.Errorf("Cannot find stream for %v", reqPath)
				return nil, errors.New("Stream Not Found")
			}

			strm := streamer.GetNetworkStream(streaming.StreamID(strmID))
			if strm == nil {
				if streamer.SelfAddress != nodeID {
					glog.Infof("Cannot find HLS stream:%v locally, forwarding request to the newtork", strmID)
					forwarder.Stream(strmID, kademlia.Address(ethCommon.HexToHash("")), lpmsStream.HLS)
				} else {
					glog.Infof("Cannot find HLS stream:%v, returning 404", strmID)
					return nil, ErrNotFound
				}
			} else {
				// glog.Infof("Found HLS stream:%v locally", strmID)
			}

			subID := "local"
			hlsBuffer := streamer.GetHLSMuxer(strmID, subID)
			if hlsBuffer == nil {
				glog.Infof("Creating new HLS buffer")
				hlsBuffer = lpmsStream.NewHLSBuffer(HLSBufferWindow, HLSBufferCap)
				err := streamer.SubscribeToHLSStream(strmID, subID, hlsBuffer)
				if err != nil {
					glog.Errorf("Error subscribing to hls stream:%v", reqPath)
					return nil, err
				}
			} else {
				// glog.Infof("Found HLS buffer for %v", reqPath)
			}
			// glog.Infof("Buffer subscribed to local stream:%v ", strmID)

			startTime := time.Now()
			for {
				// _, err := hlsBuffer.(*lpmsStream.HLSBuffer).GeneratePlaylist(0)
				_, err := hlsBuffer.(*lpmsStream.HLSBuffer).LatestPlaylist()
				if err == nil {
					hlsSubTimer[streaming.StreamID(strmID)] = time.Now()
					return hlsBuffer.(*lpmsStream.HLSBuffer), nil
				} else {
					glog.Errorf("Error generating pl: %v", err)
				}
				time.Sleep(time.Second * 2) //Sleep for 2 seconds so the segments start to get to the buffer
				if time.Since(startTime) > HLSWaitTime {
					return nil, ErrNotFound
				}
			}
		})

	server.HandleRTMPPublish(
		//getStreamID
		func(url *url.URL) (string, error) {
			return "", nil
		},
		//getStream
		func(url *url.URL) (lpmsStream.Stream, lpmsStream.Stream, error) {

			rtmpStrmID := streaming.StreamID(parseStreamID(url.Path))
			if rtmpStrmID == "" {
				rtmpStrmID = streaming.MakeStreamID(streamer.SelfAddress, fmt.Sprintf("%x", streaming.RandomStreamID()))
			}
			nodeID, _ := rtmpStrmID.SplitComponents()
			if nodeID != streamer.SelfAddress {
				glog.Errorf("Invalid rtmp strmID - nodeID component needs to be self.")
				return nil, nil, ErrStreamPublish
			}

			rtmpStream := streamer.GetNetworkStream(rtmpStrmID)
			if rtmpStream == nil {
				var rtmpErr error
				rtmpStream, rtmpErr = streamer.AddNewNetworkStream(rtmpStrmID, lpmsStream.RTMP)
				if rtmpErr != nil {
					glog.Errorf("Error when creating RTMP stream: %v", rtmpErr)
					return nil, nil, ErrStreamPublish
				}
			}

			hlsStrmID := streaming.StreamID(url.Query().Get("hlsStrmID"))
			if hlsStrmID == "" {
				hlsStrmID = streaming.MakeStreamID(streamer.SelfAddress, fmt.Sprintf("%x", streaming.RandomStreamID()))
			}
			nodeID, _ = hlsStrmID.SplitComponents()
			if nodeID != streamer.SelfAddress {
				glog.Errorf("Invalid hlsStrmID - nodeID component needs to be self.")
				return nil, nil, ErrStreamPublish
			}
			hlsStream, err := streamer.AddNewNetworkStream(hlsStrmID, lpmsStream.HLS)
			if err != nil {
				glog.Errorf("Error when creating HLS stream: %v", err)
				return nil, nil, ErrStreamPublish
			}

			glog.Infof("RTMP streamID is %v", rtmpStream.GetStreamID())
			glog.Infof("HLS streamID is %v", hlsStream.GetStreamID())

			viz.LogBroadcast(rtmpStream.GetStreamID())
			viz.LogBroadcast(hlsStream.GetStreamID())

			return rtmpStream, hlsStream, nil
		},
		//finishStream
		func(rtmpStrmID string, hlsStrmID string) {
			glog.Infof("Finish Stream - canceling stream (need to implement handler for Done())")
			streamer.DeleteNetworkStream(streaming.StreamID(rtmpStrmID))
			streamer.DeleteNetworkStream(streaming.StreamID(hlsStrmID))
			streamer.UnsubscribeAll(rtmpStrmID)
			streamer.UnsubscribeAll(hlsStrmID)
		})

	server.HandleRTMPPlay(
		//getStream
		func(ctx context.Context, reqPath string, dst av.MuxCloser) error {
			glog.Infof("Got req: ", reqPath)

			var strmID string
			regex, _ := regexp.Compile("\\/stream\\/([[:alpha:]]|\\d)*")
			match := regex.FindString(reqPath)
			if match != "" {
				strmID = strings.Replace(match, "/stream/", "", -1)
			}

			if strmID == "" {
				glog.Errorf("Cannot find stream for %v", reqPath)
				return errors.New("Stream Not Found")
			}

			// glog.Infof("Got RTMP streamID as %v", strmID)
			viz.LogConsume(strmID)

			strm := streamer.GetNetworkStream(streaming.StreamID(strmID))
			if strm == nil {
				//Send subscribe request
				glog.Infof("No local RTMP stream found - forwarding request to the network")
				forwarder.Stream(strmID, kademlia.Address(ethCommon.HexToHash("")), lpmsStream.RTMP)
			}
			q := pubsub.NewQueue()
			subID := streaming.RandomStreamID().Str()

			err := streamer.SubscribeToRTMPStream(strmID, subID, q)
			if err != nil {
				glog.Errorf("Error subscribing to stream %v", err)
				return err
			}

			ec := make(chan error)
			go func() { ec <- avutil.CopyFile(dst, q.Oldest()) }()
			select {
			case err := <-ec:
				streamer.UnsubscribeToRTMPStream(strmID, subID)
				glog.Errorf("Error copying to local player: %v", err)
				forwarder.StopStream(strmID, kademlia.Address(ethCommon.HexToHash("")), lpmsStream.RTMP) //This could fail if it's a local stream, but it's ok.
				return err
			}
		})

	http.HandleFunc("/createStream", func(w http.ResponseWriter, r *http.Request) {
		strmID := streaming.MakeStreamID(streamer.SelfAddress, fmt.Sprintf("%x", streaming.RandomStreamID()))
		newRTMPStream, _ := streamer.AddNewNetworkStream(strmID, lpmsStream.RTMP)
		res := map[string]string{"streamID": newRTMPStream.GetStreamID()}

		js, err := json.Marshal(res)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		glog.Infof("Created Stream: %s", js)
		w.Header().Set("Content-Type", "application/json")
		w.Write(js)
	})

	http.HandleFunc("/localStreams", func(w http.ResponseWriter, r *http.Request) {
		streams := streamer.GetAllNetworkStreams()
		ret := make([]map[string]string, 0, len(streams))
		for _, s := range streamer.GetAllNetworkStreams() {
			sid := streaming.StreamID(s.GetStreamID())
			nodeID, _ := sid.SplitComponents()
			var source string

			if nodeID == streamer.SelfAddress {
				source = "local"
			} else {
				source = fmt.Sprintf("%v", nodeID)
			}

			if s.Format == lpmsStream.HLS {
				ret = append(ret, map[string]string{"format": "hls", "streamID": s.GetStreamID(), "source": source})
			} else {
				ret = append(ret, map[string]string{"format": "rtmp", "streamID": s.GetStreamID(), "source": source})
			}
		}

		js, err := json.Marshal(ret)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(js)
	})

	http.HandleFunc("/peersCount", func(w http.ResponseWriter, r *http.Request) {
		c := hive.PeersCount()
		ret := make(map[string]int)
		ret["count"] = c

		js, err := json.Marshal(ret)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(js)
	})

	http.HandleFunc("/streamerStatus", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(streamer.CurrentStatus()))
	})

	fs := http.FileServer(http.Dir("static"))
	fmt.Println("Serving static files from: ", fs)
	http.Handle("/static/", http.StripPrefix("/static/", fs))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/broadcast.html", 301)
	})

	server.Start()
}

func parseStreamID(reqPath string) string {
	var strmID string
	regex, _ := regexp.Compile("\\/stream\\/([[:alpha:]]|\\d)*")
	match := regex.FindString(reqPath)
	if match != "" {
		strmID = strings.Replace(match, "/stream/", "", -1)
	}
	return strmID
}
