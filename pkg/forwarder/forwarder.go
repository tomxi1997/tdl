package forwarder

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/go-faster/errors"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"

	"github.com/iyear/tdl/pkg/dcpool"
	"github.com/iyear/tdl/pkg/logger"
	"github.com/iyear/tdl/pkg/tmedia"
	"github.com/iyear/tdl/pkg/utils"
)

//go:generate go-enum --values --names --flag --nocase

// Mode
// ENUM(direct, clone)
type Mode int

type Iter interface {
	Next(ctx context.Context) bool
	Value() *Elem
	Err() error
}

type Elem struct {
	From peers.Peer
	Msg  *tg.Message
	To   peers.Peer

	Silent bool
	DryRun bool
	Mode   Mode
}

type Options struct {
	Pool     dcpool.Pool
	PartSize int
	Iter     Iter
	Progress Progress
}

type Forwarder struct {
	sent map[[2]int64]struct{} // used to filter grouped messages which are already sent
	rand *rand.Rand
	opts Options
}

func New(opts Options) *Forwarder {
	return &Forwarder{
		sent: make(map[[2]int64]struct{}),
		rand: rand.New(rand.NewSource(time.Now().UnixNano())),
		opts: opts,
	}
}

func (f *Forwarder) Forward(ctx context.Context) error {
	for f.opts.Iter.Next(ctx) {
		elem := f.opts.Iter.Value()
		if _, ok := f.sent[f.sentTuple(elem.From, elem.Msg)]; ok {
			// skip grouped messages
			continue
		}

		if _, ok := elem.Msg.GetGroupedID(); ok {
			grouped, err := utils.Telegram.GetGroupedMessages(ctx, f.opts.Pool.Default(ctx), elem.From.InputPeer(), elem.Msg)
			if err != nil {
				continue
			}

			if err = f.forwardMessage(ctx, elem, grouped...); err != nil {
				continue
			}

			continue
		}

		if err := f.forwardMessage(ctx, elem); err != nil {
			// canceled by user, so we directly return error to stop all
			if errors.Is(err, context.Canceled) {
				return err
			}
			continue
		}
	}

	return f.opts.Iter.Err()
}

func (f *Forwarder) forwardMessage(ctx context.Context, elem *Elem, grouped ...*tg.Message) (rerr error) {
	f.opts.Progress.OnAdd(elem)
	defer func() {
		f.sent[f.sentTuple(elem.From, elem.Msg)] = struct{}{}

		// grouped message also should be marked as sent
		for _, m := range grouped {
			f.sent[f.sentTuple(elem.From, m)] = struct{}{}
		}
		f.opts.Progress.OnDone(elem, rerr)
	}()

	log := logger.From(ctx).With(
		zap.Int64("from", elem.From.ID()),
		zap.Int64("to", elem.To.ID()),
		zap.Int("message", elem.Msg.ID))

	forwardTextOnly := func(msg *tg.Message) error {
		if msg.Message == "" {
			return errors.Errorf("empty message content, skip send: %d", msg.ID)
		}
		req := &tg.MessagesSendMessageRequest{
			NoWebpage:              false,
			Silent:                 elem.Silent,
			Background:             false,
			ClearDraft:             false,
			Noforwards:             false,
			UpdateStickersetsOrder: false,
			Peer:                   elem.To.InputPeer(),
			ReplyTo:                nil,
			Message:                msg.Message,
			RandomID:               f.rand.Int63(),
			ReplyMarkup:            msg.ReplyMarkup,
			Entities:               msg.Entities,
			ScheduleDate:           0,
			SendAs:                 nil,
		}
		req.SetFlags()

		if _, err := f.forwardClient(ctx, elem).MessagesSendMessage(ctx, req); err != nil {
			return errors.Wrap(err, "send message")
		}
		return nil
	}

	convForwardedMedia := func(msg *tg.Message) (tg.InputMediaClass, error) {
		if _, hasMedia := msg.GetMedia(); !hasMedia {
			// media can't be forwarded via simple copy(it depends on the server ids)
			// if it's not a media message, just break and send text copy
			return nil, errors.Errorf("message %d is not a media message", msg.ID)
		}

		// if it's a media message, but it's not protected, convert it to InputMediaClass
		// or if it's protected, but it doesn't contain photo or document,

		// we should clone photo and document via re-upload, it will be banned if we forward it directly.
		// but other media can be forwarded directly via copy
		if (!protectedDialog(elem.From) && !protectedMessage(msg)) || !photoOrDocument(msg.Media) {
			media, ok := tmedia.ConvInputMedia(msg.Media)
			if !ok {
				return nil, errors.Errorf("can't convert message %d to input class directly", msg.ID)
			}
			return media, nil
		}

		media, ok := tmedia.GetMedia(msg)
		if !ok {
			log.Warn("Can't get media from message",
				zap.Int64("peer", elem.From.ID()),
				zap.Int("message", msg.ID))

			// unsupported re-upload media
			return nil, errors.Errorf("unsupported media %T", msg.Media)
		}

		mediaFile, err := f.CloneMedia(ctx, CloneOptions{
			Media:    media,
			PartSize: f.opts.PartSize,
			Progress: uploadProgress{
				elem:     elem,
				progress: f.opts.Progress,
			},
		}, elem.DryRun)
		if err != nil {
			return nil, errors.Wrap(err, "clone media")
		}

		// now we only have to process cloned photo or document
		switch m := msg.Media.(type) {
		case *tg.MessageMediaPhoto:
			photo := &tg.InputMediaUploadedPhoto{
				Spoiler:    m.Spoiler,
				File:       mediaFile,
				TTLSeconds: m.TTLSeconds,
			}
			photo.SetFlags()
			return photo, nil
		case *tg.MessageMediaDocument:
			doc, ok := m.Document.AsNotEmpty()
			if !ok {
				return nil, errors.Errorf("empty document %d", msg.ID)
			}

			thumb, ok := tmedia.GetDocumentThumb(doc)
			if !ok {
				return nil, errors.Errorf("empty document thumb %d", msg.ID)
			}

			thumbFile, err := f.CloneMedia(ctx, CloneOptions{
				Media:    thumb,
				PartSize: f.opts.PartSize,
				Progress: nopProgress{},
			}, elem.DryRun)
			if err != nil {
				return nil, errors.Wrap(err, "clone thumb")
			}

			document := &tg.InputMediaUploadedDocument{
				NosoundVideo: false, // do not set
				ForceFile:    false, // do not set
				Spoiler:      m.Spoiler,
				File:         mediaFile,
				Thumb:        thumbFile,
				MimeType:     doc.MimeType,
				Attributes:   doc.Attributes,
				Stickers:     nil, // do not set
				TTLSeconds:   m.TTLSeconds,
			}
			document.SetFlags()

			return document, nil
		default:
			return nil, errors.Errorf("unsupported media %T", msg.Media)
		}
	}

	switch elem.Mode {
	case ModeDirect:
		// it can be forwarded via API
		if !protectedDialog(elem.From) && !protectedMessage(elem.Msg) {
			builder := message.NewSender(f.forwardClient(ctx, elem)).
				To(elem.To.InputPeer()).CloneBuilder()
			if elem.Silent {
				builder = builder.Silent()
			}

			if len(grouped) > 0 {
				ids := make([]int, 0, len(grouped))
				for _, m := range grouped {
					ids = append(ids, m.ID)
				}

				if _, err := builder.ForwardIDs(elem.From.InputPeer(), ids[0], ids[1:]...).Send(ctx); err != nil {
					goto fallback
				}

				return nil
			}

			if _, err := builder.ForwardIDs(elem.From.InputPeer(), elem.Msg.ID).Send(ctx); err != nil {
				goto fallback
			}
			return nil
		}
	fallback:
		fallthrough
	case ModeClone:
		if len(grouped) > 0 {
			media := make([]tg.InputSingleMedia, 0, len(grouped))
			for _, gm := range grouped {
				m, err := convForwardedMedia(gm)
				if err != nil {
					log.Debug("Can't convert forwarded media", zap.Error(err))
					continue
				}

				single := tg.InputSingleMedia{
					Media:    m,
					RandomID: f.rand.Int63(),
					Message:  gm.Message,
					Entities: gm.Entities,
				}
				single.SetFlags()
				media = append(media, single)
			}

			if len(media) > 0 {
				req := &tg.MessagesSendMultiMediaRequest{
					Silent:                 elem.Silent,
					Background:             false,
					ClearDraft:             false,
					Noforwards:             false,
					UpdateStickersetsOrder: false,
					Peer:                   elem.To.InputPeer(),
					ReplyTo:                nil,
					MultiMedia:             media,
					ScheduleDate:           0,
					SendAs:                 nil,
				}
				req.SetFlags()
				if _, err := f.forwardClient(ctx, elem).MessagesSendMultiMedia(ctx, req); err != nil {
					return errors.Wrap(err, "send multi media")
				}
				return nil
			}

			return forwardTextOnly(elem.Msg)
		}

		media, err := convForwardedMedia(elem.Msg)
		if err != nil {
			log.Debug("Can't convert forwarded media", zap.Error(err))
			return forwardTextOnly(elem.Msg)
		}
		// send text copy with forwarded media
		req := &tg.MessagesSendMediaRequest{
			Silent:                 elem.Silent,
			Background:             false,
			ClearDraft:             false,
			Noforwards:             false,
			UpdateStickersetsOrder: false,
			Peer:                   elem.To.InputPeer(),
			ReplyTo:                nil,
			Media:                  media,
			Message:                elem.Msg.Message,
			RandomID:               rand.Int63(),
			ReplyMarkup:            elem.Msg.ReplyMarkup,
			Entities:               elem.Msg.Entities,
			ScheduleDate:           0,
			SendAs:                 nil,
		}
		req.SetFlags()

		if _, err := f.forwardClient(ctx, elem).MessagesSendMedia(ctx, req); err != nil {
			return errors.Wrap(err, "send single media")
		}
		return nil
	}

	return fmt.Errorf("unknown mode: %s", elem.Mode)
}

func (f *Forwarder) sentTuple(peer peers.Peer, msg *tg.Message) [2]int64 {
	return [2]int64{peer.ID(), int64(msg.ID)}
}

type nopInvoker struct{}

func (n nopInvoker) Invoke(_ context.Context, _ bin.Encoder, _ bin.Decoder) error {
	return nil
}

func (f *Forwarder) forwardClient(ctx context.Context, elem *Elem) *tg.Client {
	if elem.DryRun {
		return tg.NewClient(nopInvoker{})
	}

	return f.opts.Pool.Default(ctx)
}

func protectedDialog(peer peers.Peer) bool {
	switch p := peer.(type) {
	case peers.Chat:
		return p.Raw().GetNoforwards()
	case peers.Channel:
		return p.Raw().GetNoforwards()
	}

	return false
}

func protectedMessage(msg *tg.Message) bool {
	return msg.GetNoforwards()
}

func photoOrDocument(media tg.MessageMediaClass) bool {
	switch media.(type) {
	case *tg.MessageMediaPhoto, *tg.MessageMediaDocument:
		return true
	default:
		return false
	}
}
