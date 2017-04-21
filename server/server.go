package server

import (
	"context"
	"fmt"
	"time"

	"github.com/CyCoreSystems/ari"
	"github.com/CyCoreSystems/ari-proxy/proxy"
	"github.com/CyCoreSystems/ari-proxy/server/dialog"
	"github.com/CyCoreSystems/ari/client/native"
	"github.com/nats-io/nats"
	"github.com/pkg/errors"
	log15 "gopkg.in/inconshreveable/log15.v2"
)

// Server describes the asterisk-facing ARI proxy server
type Server struct {
	// Application is the name of the ARI application of this server
	Application string

	// AsteriskID is the unique identifier for the Asterisk box
	// to which this server is connected.
	AsteriskID string

	// NATSPrefix is the string which should be prepended to all NATS subjects, sending and receiving.  It defaults to "ari.".
	NATSPrefix string

	// ari is the native Asterisk ARI client by which this proxy is directly connected
	ari ari.Client

	// nats is the JSON-encoded NATS connection
	nats *nats.EncodedConn

	// Dialog is the dialog manager
	Dialog dialog.Manager

	readyCh chan struct{}

	// cancel is the context cancel function, by which all subtended subscriptions may be terminated
	cancel context.CancelFunc

	// Log is the log15.Logger for the service.  You may replace or call SetHandler() on this at any time to change the logging of the service.
	Log log15.Logger
}

// New returns a new Server
func New() *Server {
	log := log15.New()
	log.SetHandler(log15.DiscardHandler())

	return &Server{
		NATSPrefix: "ari.",
		readyCh:    make(chan struct{}),
		Dialog:     dialog.NewMemManager(),
		Log:        log,
	}
}

// Listen runs the given server, listening to ARI and NATS, as specified
func (s *Server) Listen(ctx context.Context, ariOpts native.Options, natsURI string) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	// Connect to ARI
	s.ari, err = native.Connect(ctx, ariOpts)
	if err != nil {
		return errors.Wrap(err, "failed to connect to ARI")
	}
	defer s.ari.Close()

	// Connect to NATS
	nc, err := nats.Connect(natsURI)
	if err != nil {
		return errors.Wrap(err, "failed to connect to NATS")
	}
	s.nats, err = nats.NewEncodedConn(nc, nats.JSON_ENCODER)
	if err != nil {
		return errors.Wrap(err, "failed to encode NATS connection")
	}
	defer s.nats.Close()

	return s.listen(ctx)
}

// ListenOn runs the given server, listening on the provided ARI and NATS connections
func (s *Server) ListenOn(ctx context.Context, a ari.Client, n *nats.EncodedConn) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	s.ari = a
	s.nats = n

	return s.listen(ctx)
}

// Ready returns a channel which is closed when the Server is ready
func (s *Server) Ready() <-chan struct{} {
	if s.readyCh == nil {
		s.readyCh = make(chan struct{})
	}
	return s.readyCh
}

// nolint: gocyclo
func (s *Server) listen(ctx context.Context) error {

	var wg closeGroup
	defer func() {
		select {
		case <-wg.Done():
		case <-time.After(500 * time.Millisecond):
			panic("timeout waiting for shutdown of sub components")
		}
	}()

	// First, get the Asterisk ID

	ret, err := s.ari.Asterisk().Info("")
	if err != nil {
		return errors.Wrap(err, "failed to get Asterisk ID")
	}

	s.AsteriskID = ret.SystemInfo.EntityID
	if s.AsteriskID == "" {
		return errors.New("empty Asterisk ID")
	}

	// Store the ARI application name for top-level access
	s.Application = s.ari.ApplicationName()

	//
	// Listen on the initial NATS subjects
	//

	// ping handler
	pingSub, err := s.nats.Subscribe(proxy.PingSubject(s.NATSPrefix), s.pingHandler)
	if err != nil {
		return errors.Wrap(err, "failed to subscribe to pings")
	}
	defer wg.Add(pingSub.Unsubscribe)

	// get a contextualized request handler
	requestHandler := s.newRequestHandler(ctx)

	// get handlers
	allGet, err := s.nats.Subscribe(proxy.Subject(s.NATSPrefix, "get", "", ""), requestHandler)
	if err != nil {
		return errors.Wrap(err, "failed to create get-all subscription")
	}
	defer wg.Add(allGet.Unsubscribe)()

	appGet, err := s.nats.Subscribe(proxy.Subject(s.NATSPrefix, "get", s.Application, ""), requestHandler)
	if err != nil {
		return errors.Wrap(err, "failed to create get-app subscription")
	}
	defer wg.Add(appGet.Unsubscribe)()
	idGet, err := s.nats.Subscribe(proxy.Subject(s.NATSPrefix, "get", s.Application, s.AsteriskID), requestHandler)
	if err != nil {
		return errors.Wrap(err, "failed to create get-id subscription")
	}
	defer wg.Add(idGet.Unsubscribe)()

	// data handlers
	allData, err := s.nats.Subscribe(proxy.Subject(s.NATSPrefix, "data", "", ""), requestHandler)
	if err != nil {
		return errors.Wrap(err, "failed to create data-all subscription")
	}
	defer wg.Add(allData.Unsubscribe)()
	appData, err := s.nats.Subscribe(proxy.Subject(s.NATSPrefix, "data", s.Application, ""), requestHandler)
	if err != nil {
		return errors.Wrap(err, "failed to create data-app subscription")
	}
	defer wg.Add(appData.Unsubscribe)()
	idData, err := s.nats.Subscribe(proxy.Subject(s.NATSPrefix, "data", s.Application, s.AsteriskID), requestHandler)
	if err != nil {
		return errors.Wrap(err, "failed to create data-id subscription")
	}
	defer wg.Add(idData.Unsubscribe)()

	// command handlers
	allCommand, err := s.nats.Subscribe(proxy.Subject(s.NATSPrefix, "command", "", ""), requestHandler)
	if err != nil {
		return errors.Wrap(err, "failed to create command-all subscription")
	}
	defer wg.Add(allCommand.Unsubscribe)()
	appCommand, err := s.nats.Subscribe(proxy.Subject(s.NATSPrefix, "command", s.Application, ""), requestHandler)
	if err != nil {
		return errors.Wrap(err, "failed to create command-app subscription")
	}
	defer wg.Add(appCommand.Unsubscribe)()
	idCommand, err := s.nats.Subscribe(proxy.Subject(s.NATSPrefix, "command", s.Application, s.AsteriskID), requestHandler)
	if err != nil {
		return errors.Wrap(err, "failed to create command-id subscription")
	}
	defer wg.Add(idCommand.Unsubscribe)()

	// create handlers
	allCreate, err := s.nats.QueueSubscribe(proxy.Subject(s.NATSPrefix, "create", "", ""), "ariproxy", requestHandler)
	if err != nil {
		return errors.Wrap(err, "failed to create create-all subscription")
	}
	defer wg.Add(allCreate.Unsubscribe)()
	appCreate, err := s.nats.QueueSubscribe(proxy.Subject(s.NATSPrefix, "create", s.Application, ""), "ariproxy", requestHandler)
	if err != nil {
		return errors.Wrap(err, "failed to create create-app subscription")
	}
	defer wg.Add(appCreate.Unsubscribe)()
	idCreate, err := s.nats.QueueSubscribe(proxy.Subject(s.NATSPrefix, "create", s.Application, s.AsteriskID), "ariproxy", requestHandler)
	if err != nil {
		return errors.Wrap(err, "failed to create create-id subscription")
	}
	defer wg.Add(idCreate.Unsubscribe)()

	// Run the periodic announcer
	go s.runAnnouncer(ctx)

	// Run the event handler
	go s.runEventHandler(ctx)

	// TODO: run the dialog cleanup routine (remove bindings for entities which no longer exist)
	//go s.runDialogCleaner(ctx)

	// Close the readyChannel to indicate that we are operational
	if s.readyCh != nil {
		close(s.readyCh)
	}

	// Wait for context closure to exit
	<-ctx.Done()
	return ctx.Err()
}

// runAnnouncer runs the periodic discovery announcer
func (s *Server) runAnnouncer(ctx context.Context) {
	ticker := time.NewTicker(proxy.AnnouncementInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.announce()
		}
	}
}

// announce publishes the presence of this server to the cluster
func (s *Server) announce() {
	s.nats.Publish(proxy.AnnouncementSubject(s.NATSPrefix), &proxy.Announcement{
		Node:        s.AsteriskID,
		Application: s.Application,
	})
}

// runEventHandler processes events which are received from ARI
func (s *Server) runEventHandler(ctx context.Context) {
	sub := s.ari.Bus().Subscribe(ari.Events.All)
	defer sub.Cancel()

	for {
		select {
		case <-ctx.Done():
			return
		case e := <-sub.Events():
			raw := ari.EventToRaw(e)
			if raw == nil {
				s.Log.Error("Failed to encode event to RawEvent")
				continue
			}

			// Add metadata
			raw.Header.Set("application", s.Application)
			raw.Header.Set("asterisk", s.AsteriskID)

			// Publish event to canonical destination
			s.nats.Publish(fmt.Sprintf("%sevent.%s.%s", s.NATSPrefix, s.Application, s.AsteriskID), raw)

			// Publish event to any associated dialogs
			for _, d := range s.dialogsForEvent(e) {
				dRaw := raw
				dRaw.Header.Set("dialog", d)
				s.nats.Publish(fmt.Sprintf("%sdialogevent.%s", s.NATSPrefix, d), dRaw)
			}
		}
	}
}

// pingHandler publishes the server's presence
func (s *Server) pingHandler(m *nats.Msg) {
	s.announce()
}

// newRequestHandler returns a context-wrapped nats.Handler to handle requests
func (s *Server) newRequestHandler(ctx context.Context) func(subject string, reply string, req *proxy.Request) {
	return func(subject string, reply string, req *proxy.Request) {
		go s.dispatchRequest(ctx, reply, req)
	}
}

func (s *Server) dispatchRequest(ctx context.Context, reply string, req *proxy.Request) {
	var f func(context.Context, string, *proxy.Request)

	switch req.Kind {
	case "ApplicationData":
		f = s.applicationData
	case "ApplicationList":
		f = s.applicationList
	case "ApplicationSubscribe":
		f = s.applicationSubscribe
	case "ApplicationUnsubscribe":
		f = s.applicationUnsubscribe
	case "AsteriskInfo":
		f = s.asteriskInfo
	case "AsteriskReloadModule":
		f = s.asteriskReloadModule
	case "AsteriskVariableGet":
		f = s.asteriskVariableGet
	case "AsteriskVariableSet":
		f = s.asteriskVariableSet
	case "BridgeAddChannel":
		f = s.bridgeAddChannel
	case "BridgeCreate":
		f = s.bridgeCreate
	case "BridgeData":
		f = s.bridgeData
	case "BridgeDelete":
		f = s.bridgeDelete
	case "BridgeList":
		f = s.bridgeList
	case "BridgePlay":
		f = s.bridgePlay
	case "BridgeRecord":
		f = s.bridgeRecord
	case "BridgeRemoveChannel":
		f = s.bridgeRemoveChannel
	case "BridgeSubscribe":
		f = s.bridgeSubscribe
	case "BridgeUnsubscribe":
		f = s.bridgeUnsubscribe
	case "ChannelAnswer":
		f = s.channelAnswer
	case "ChannelBusy":
		f = s.channelBusy
	case "ChannelCongestion":
		f = s.channelCongestion
	case "ChannelCreate":
		f = s.channelCreate
	case "ChannelContinue":
		f = s.channelContinue
	case "ChannelData":
		f = s.channelData
	case "ChannelDial":
		f = s.channelDial
	case "ChannelHangup":
		f = s.channelHangup
	case "ChannelHold":
		f = s.channelHold
	case "ChannelList":
		f = s.channelList
	case "ChannelMOH":
		f = s.channelMOH
	case "ChannelMute":
		f = s.channelMute
	case "ChannelOriginate":
		f = s.channelOriginate
	case "ChannelPlay":
		f = s.channelPlay
	case "ChannelRecord":
		f = s.channelRecord
	case "ChannelRing":
		f = s.channelRing
	case "ChannelSendDTMF":
		f = s.channelSendDTMF
	case "ChannelSilence":
		f = s.channelSilence
	case "ChannelSnoop":
		f = s.channelSnoop
	case "ChannelStopHold":
		f = s.channelStopHold
	case "ChannelStopMOH":
		f = s.channelStopMOH
	case "ChannelStopRing":
		f = s.channelStopRing
	case "ChannelStopSilence":
		f = s.channelStopSilence
	case "ChannelSubscribe":
		f = s.channelSubscribe
	case "ChannelUnmute":
		f = s.channelUnmute
	case "ChannelVariableGet":
		f = s.channelVariableGet
	case "ChannelVariableSet":
		f = s.channelVariableSet
	case "DeviceStateData":
		f = s.deviceStateData
	case "DeviceStateDelete":
		f = s.deviceStateDelete
	case "DeviceStateList":
		f = s.deviceStateList
	case "DeviceStateUpdate":
		f = s.deviceStateUpdate
	case "EndpointData":
		f = s.endpointData
	case "EndpointList":
		f = s.endpointList
	case "EndpointListByTech":
		f = s.endpointListByTech
	case "MailboxData":
		f = s.mailboxData
	case "MailboxDelete":
		f = s.mailboxDelete
	case "MailboxList":
		f = s.mailboxList
	case "MailboxUpdate":
		f = s.mailboxUpdate
	case "PlaybackControl":
		f = s.playbackControl
	case "PlaybackData":
		f = s.playbackData
	case "PlaybackStop":
		f = s.playbackControl
	case "PlaybackStop":
		f = s.playbackControl
	case "PlaybackSubscribe":
		f = s.playbackSubscribe
	case "RecordingStoredCopy":
		f = s.recordingStoredCopy
	case "RecordingStoredData":
		f = s.recordingStoredData
	case "RecordingStoredDelete":
		f = s.recordingStoredDelete
	case "RecordingStoredList":
		f = s.recordingStoredList
	case "RecordingLiveData":
		f = s.recordingLiveData
	case "RecordingLiveDelete":
		f = s.recordingLiveDelete
	case "RecordingLiveMute":
		f = s.recordingLiveMute
	case "RecordingLivePause":
		f = s.recordingLivePause
	case "RecordingLiveResume":
		f = s.recordingLiveResume
	case "RecordingLiveScrap":
		f = s.recordingLiveScrap
	case "RecordingLiveStop":
		f = s.recordingLiveStop
	case "RecordingLiveUnmute":
		f = s.recordingLiveUnmute
	case "SoundData":
		f = s.soundData
	case "SoundList":
		f = s.soundList
	case "AsteriskConfigData":
		f = s.asteriskConfigData
	case "AsteriskConfigDelete":
		f = s.asteriskConfigDelete
	case "AsteriskConfigUpdate":
		f = s.asteriskConfigUpdate
	case "AsteriskLoggingCreate":
		f = s.asteriskLoggingCreate
	case "AsteriskLoggingData":
		f = s.asteriskLoggingData
	case "AsteriskLoggingDelete":
		f = s.asteriskLoggingDelete
	case "AsteriskLoggingList":
		f = s.asteriskLoggingList
	case "AsteriskLoggingRotate":
		f = s.asteriskLoggingRotate
	case "AsteriskModuleData":
		f = s.asteriskModuleData
	case "AsteriskModuleLoad":
		f = s.asteriskModuleLoad
	case "AsteriskModuleList":
		f = s.asteriskModuleList
	case "AsteriskModuleReload":
		f = s.asteriskModuleReload
	case "AsteriskModuleUnload":
		f = s.asteriskModuleUnload
	default:
		f = func(ctx context.Context, reply string, req *proxy.Request) {
			s.sendError(reply, errors.New("Not implemented"))
		}
	}

	f(ctx, reply, req)
}

func (s *Server) sendError(reply string, err error) {
	s.nats.Publish(reply, proxy.NewErrorResponse(err))
}

func (s *Server) sendNotFound(reply string) {
	s.nats.Publish(reply, proxy.NewErrorResponse(proxy.ErrNotFound))
}

// Metadata returns the metadata for this server.  The dialog parameter is
// optional; set it to the empty string if one is not applicable or known.
func (s *Server) Metadata(dialog string) *proxy.Metadata {
	return &proxy.Metadata{
		Application: s.Application,
		Node:        s.AsteriskID,
		Dialog:      dialog,
	}
}

/*
// Start runs the server side instance
func (i *Instance) Start(ctx context.Context) {
	i.ctx, i.cancel = context.WithCancel(ctx)

	i.log.Debug("Starting dialog instance")

	go func() {
		i.application()
		i.asterisk()
		i.modules()
		i.channel()
		i.storedRecording()
		i.liveRecording()
		i.bridge()
		i.device()
		i.playback()
		i.mailbox()
		i.sound()
		i.logging()
		i.config()

		// do commands last, since that is the one that will be dispatching
		i.commands()

		close(i.readyCh)

		<-i.ctx.Done()
	}()

	<-i.readyCh
}

// Stop stops the instance
func (i *Instance) Stop() {
	if i == nil {
		return
	}
	i.cancel()
}

func (i *Instance) String() string {
	return fmt.Sprintf("Instance{%s}", i.Dialog.ID)
}
*/
