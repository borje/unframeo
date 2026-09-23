// SPDX-License-Identifier: GPL-3.0-or-later

// Package frameo speaks the message protocol a Frameo photo frame uses over a
// SecureDeviceGrid connection.
package frameo

import (
	"fmt"

	"github.com/borje/unframeo/internal/frameo/pb"
)

// Every message is framed as two big-endian 32-bit integers followed by a
// protobuf: a constant, then the type that says which protobuf it is.
const (
	// framePrefix precedes every message. The frame reads it but dispatches
	// only on the type that follows.
	framePrefix = 18
	frameHeader = 8
)

// Message types. These are the values the frame dispatches on.
const (
	TypeGetInfo            = 1
	TypeFrameInfo          = 2
	TypeMedia              = 4
	TypeMediaDataSegment   = 5
	TypeAcknowledgeReceipt = 6
	TypePairingCode        = 8
	TypeReaction           = 10
	TypeEncryptedData      = 11
	TypeBackupStatus       = 19
	TypeRestoreStatus      = 22
	TypeMultiPartMessage   = 30
	TypeAllMediaMetaData   = 32
	TypeCalendarStatuses   = 40

	// TypeGetAllMediaMetaData requests a listing. It was a guess, reasoned from
	// the one-below-its-answer pattern seen elsewhere in this protocol
	// (GetInfo/FrameInfo at 1/2, and the same gap at 7/8, 18/19, 21/22, 42/43,
	// 47/48) applied to AllMediaMetaData's 32. Confirmed against a real frame:
	// sending 31 draws a genuine TypeAllMediaMetaData reply, which a frame only
	// sends in answer to this request. (24, reasoned the same way from
	// AllMediaIds at 25, draws a reply typed 25 instead — a different,
	// unimplemented request, not this one.)
	TypeGetAllMediaMetaData = 31

	// TypeDeleteMedia removes photos from the frame for good. Confirmed against
	// a real frame on a throwaway photo: a genuine DeleteMedia{MediaIds,
	// RequiresAcknowledgeReceiptId} sent to 34 drew an AcknowledgeReceipt
	// carrying our own acknowledge id and no error, and the photo was gone from
	// the next listing rather than present and hidden.
	TypeDeleteMedia = 34

	// TypeChangeMediaVisibility hides and shows a photo without deleting it.
	// Confirmed against a real frame, in both directions, on a throwaway photo
	// uploaded for the purpose: sending 33 with a genuine
	// ChangeMediaVisibility{MediaIds, IsVisible} drew an AcknowledgeReceipt
	// carrying our own acknowledge id and no error, and the listing then
	// reported that id as isVisible=false; sending it again with IsVisible
	// true moved it back. Hidden photos stay in the listing, marked hidden,
	// which is what makes this observable at all.
	TypeChangeMediaVisibility = 33

	// TypeGetMedia asks the frame to send one stored photo back. The number
	// came from a decompile of the app rather than from a probe:
	// net.frameo.app v1.40.5 builds a GetMedia and sends it on 23, and the
	// frame answers with a Media(4) header followed by MediaDataSegment(5)
	// bytes -- the same pair the upload path uses, pointed the other way. See
	// GETMEDIA.md.
	//
	// Since confirmed against a real frame, over a direct local connection and
	// over the relay alike: a GetMedia sent to 23 brought back a photo that opens,
	// with the byte count the header announced. The header also answered what
	// GETMEDIA.md could only infer -- the frame echoes the id it was asked
	// for, dates the photo, and describes no extra streams at all, so a
	// full-resolution reply does not append the thumbnail. Asking for a scaled
	// copy is the same in that respect: one stream, no extras.
	//
	// It is not the same in any other respect, and the width and height in the
	// request are not the scaling knob they look like. The frame holds two
	// copies of a photo and the bound only chooses between them, at a boundary
	// measured at exactly 500. Size records what that cost to find out.
	TypeGetMedia = 23

	// TypeClientInfo tells the frame who is calling: ClientInfo{Name}. The
	// frame asks first -- every connection begins with a GetInfo(1) from the
	// frame's side, the baseline chatter the probes below had to subtract --
	// and this is the answer, which is where the frame gets the sender's name
	// it shows beside a photo. The number and the exchange are read from
	// yasoob/frameo-client, which replies to an inbound 1 with a 3 carrying
	// field 1 as a string; not yet confirmed against a real frame by watching
	// the name appear on it.
	TypeClientInfo = 3

	// TypeRequestPermission asks the frame's owner to grant this client a
	// permission: RequestPermission{Permission}, where 1 asks to view photos
	// and 3 to manage them. The frame shows an Allow prompt and answers
	// nothing over the wire; the grant shows up in the next FrameInfo's
	// permission flags, so a client polls GetInfo until it does. Also read
	// from yasoob/frameo-client, which treats 3 as covering view as well: its
	// manage check waits for both flags. Not yet confirmed against a real
	// frame.
	TypeRequestPermission = 27
)

// Permission is something the frame's owner can grant a client.
type Permission int32

const (
	// PermissionView allows listing and fetching photos.
	PermissionView Permission = 1
	// PermissionManage allows hiding, showing and deleting photos, and
	// implies PermissionView.
	PermissionManage Permission = 3
)

func (p Permission) String() string {
	switch p {
	case PermissionView:
		return "view photos"
	case PermissionManage:
		return "manage photos"
	default:
		return fmt.Sprintf("permission %d", int32(p))
	}
}

// Granted reports whether info shows p as held. Manage counts only when view
// is held as well, since one is no use without the other.
func Granted(info *pb.FrameInfo, p Permission) bool {
	switch p {
	case PermissionView:
		return info.GetHasPermissionViewPhotos()
	case PermissionManage:
		return info.GetHasPermissionViewPhotos() && info.GetHasPermissionManagePhotos()
	default:
		return false
	}
}

// Candidates seen but not yet confirmed the way TypeGetAllMediaMetaData was:
// each drew a distinguishable, real reply from the frame rather than being
// ignored like an arbitrary number, but every reply so far has been the same
// permission-denied refusal `list` gets (error code 5, from `May manage:
// false` on this pairing), never a genuine positive payload. Naming these as
// constants waits on that positive confirmation.
//
//   - 23 is no longer on this list: it is TypeGetMedia above, confirmed since
//     by a fetch that brought back a photo. What this probe saw is still
//     worth recording, because it agrees: an empty payload sent to 23 drew a
//     `Media`-shaped message carrying `Error{Code: 5}` at field 11, matching
//     Media's own error field exactly (payload hex 5a020805 decodes to field
//     11 → {field 1: 5}). A number that answers in Media's shape is a number
//     that deals in Media, which is what GetMedia does.
//   - 24, empty payload: replies typed 25 carrying `Error{Code: 5}` at field
//     2 (payload hex 12020805 decodes to field 2 → {field 1: 5}) — a
//     smaller, distinct shape from Media's, consistent with a lean
//     "AllMediaIds" (ids plus an error field, nothing else). This exact
//     payload also turned up typed as AcknowledgeReceipt(6) in a concurrent
//     probe from another session sharing this identity, which briefly looked
//     like cross-talk between the two connections. It isn't: `Error` sits at
//     field 2 in several message types here (AcknowledgeReceipt,
//     AllMediaMetaData, and by the same pattern 25), so a bare
//     `Error{Code: 5}` refusal serializes identically regardless of which of
//     them wraps it — the frame header's type number is what distinguishes
//     them, not the payload. Still, avoid running live probes against the
//     same paired frame from two sessions at once; it wastes effort even
//     when it doesn't produce a genuinely ambiguous result.
//     (34 was on this list too, as a MediaUpdate candidate. It is not one: it
//     deletes, and is now TypeDeleteMedia above. The MediaUpdate payload that
//     first drew a reply from it was being read as a deletion the whole time,
//     for the reason set out below.)
//   - 26, empty payload: drew no reply at all beyond the baseline GetInfo
//     chatter every connection gets (verified by comparison against a
//     nonsense message number, which gets the same baseline and nothing
//     more). No evidence it exists as its own request; it may only ever
//     appear as part of an actual multi-id listing reply, or the existing
//     generic MultiPartMessage(30) wrapper may already cover that case and
//     this number is unused.
//
// The 23/24/26 probes above were empty-payload and made while this pairing was
// still refused for lack of view/manage permission. That permission has since
// been granted on the frame, so their error-code-5 results say nothing about
// those numbers any more and 24 and 26 are both worth re-probing. 23 no longer
// needs a probe so much as a use: `unframeo get <id>` sends a real GetMedia with
// a real media id, which is the experiment that comment used to ask for.
//
// Probing for MediaUpdate's number is dangerous in a way the others are not,
// and the reason is the wire format rather than anything about the frame.
// MediaUpdate is {Media media = 1}, a length-delimited field 1. DeleteMedia is
// {repeated sint64 mediaIds = 1}, and a packed repeated field is *also*
// length-delimited field 1, so a DeleteMedia parser reads the Media submessage
// as a packed zigzag array — and the media id inside it, being a zigzag varint
// already, decodes back to itself. A MediaUpdate probe therefore reads as a
// deletion naming the very photo it was meant to edit, which is exactly how
// the 34 result above came about. The same collision makes a hide request
// indistinguishable from a deletion: IsVisible is false by default and proto3
// omits default values, so ChangeMediaVisibility{ids, IsVisible: false} and
// DeleteMedia{ids} serialise to identical bytes. Setting IsVisible true does
// not help either; DeleteMedia simply ignores the extra field.
//
// GetMedia joins that family rather than escaping it, and is worth singling out
// because it now has a command behind it. GetMedia is {sint64 mediaId = 1}: one
// unpacked varint in field 1. A repeated scalar field accepts both the packed
// and the unpacked encoding, so those exact bytes are also a valid
// DeleteMedia{mediaIds: [that id]}. Checked rather than reasoned: GetMedia{111}
// serialises to 08de01, and that decodes back as DeleteMedia{mediaIds: [111]}.
// `unframeo -type 34 get <id>` therefore deletes the photo it was asked to fetch,
// and reads as an ordinary deletion at the other end.
//
// So there is no payload that is safe to aim at an unknown number. The only
// protection is the media id: probe with a throwaway photo uploaded for the
// purpose and never put a wanted photo's id in an experimental request.

// MediaUpdate's number is the one this client still needs and does not have.
// It edits an already-uploaded photo's caption, crop and capture date in
// place, and nothing above sends it: the app has no send site for it anywhere
// in a decompile of v1.40.5, so there is no source to read the number from.
// Find it by probing against a throwaway photo and watching the listing's
// captureDate, which is the only field of a MediaUpdate the listing reports
// back — but read the collision note above first, because a MediaUpdate probe
// aimed at the wrong number reads as a deletion of the photo it names.
const TypeMediaUpdate = 0

// typeName labels a message type for logs and errors.
func typeName(t int32) string {
	switch t {
	case TypeGetInfo:
		return "GetInfo"
	case TypeFrameInfo:
		return "FrameInfo"
	case TypeClientInfo:
		return "ClientInfo"
	case TypeRequestPermission:
		return "RequestPermission"
	case TypeMedia:
		return "Media"
	case TypeMediaDataSegment:
		return "MediaDataSegment"
	case TypeAcknowledgeReceipt:
		return "AcknowledgeReceipt"
	case TypePairingCode:
		return "PairingCode"
	case TypeReaction:
		return "Reaction"
	case TypeEncryptedData:
		return "EncryptedData"
	case TypeBackupStatus:
		return "BackupStatus"
	case TypeRestoreStatus:
		return "RestoreStatus"
	case TypeMultiPartMessage:
		return "MultiPartMessage"
	case TypeAllMediaMetaData:
		return "AllMediaMetaData"
	case TypeGetAllMediaMetaData:
		return "GetAllMediaMetaData"
	case TypeGetMedia:
		return "GetMedia"
	case TypeDeleteMedia:
		return "DeleteMedia"
	case TypeChangeMediaVisibility:
		return "ChangeMediaVisibility"
	case TypeCalendarStatuses:
		return "CalendarStatuses"
	default:
		return "unknown"
	}
}
