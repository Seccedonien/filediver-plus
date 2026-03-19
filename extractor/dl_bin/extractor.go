// Package dl_bin implements a filediver extractor plugin for Helldivers 2
// dl_bin (armour-set / customization-kit binary blob) files.
//
// Two export modes are provided:
//
//   - ExtractDlBinJSON  – parses the binary and writes a human-readable
//     pretty-printed JSON file.
//   - ExtractDlBinRaw   – dumps the raw main-data blob verbatim (useful when
//     the JSON parser cannot handle a newer variant of the format after a
//     game update that adds new fields).
//
// # Registration
//
// Three edits are required to wire this plugin into the filediver app:
//
//  1. extractor/dl_bin/extractor.go – this file (already done).
//
//  2. app/app.go – add a case to the switch in ExtractFile():
//
//     import extr_dl_bin "github.com/xypwn/filediver/extractor/dl_bin"
//
//     case "dl_bin":
//     if extrFormat == "raw" {
//     extr = extr_dl_bin.ExtractDlBinRaw
//     } else {
//     extr = extr_dl_bin.ExtractDlBinJSON
//     }
//
//  3. app/appconfig/appconfig.go – register the type and add a config section:
//
//     // In Extractable map:
//     "dl_bin": true,
//
//     // New struct field in Config (alongside the other file-type sections):
//     DlBin struct {
//     Format string `cfg:"options=json,raw"`
//     } `cfg:"tags=t:dl_bin help='armour-set / customization-kit data'"`
package dl_bin

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"github.com/xypwn/filediver/extractor"
	"github.com/xypwn/filediver/stingray"
)

// ---------------------------------------------------------------------------
// Binary-format constants
// ---------------------------------------------------------------------------

// armorSetBaseOffset is the base address used for in-file pointer arithmetic.
// Pointers stored in the binary are absolute addresses; subtract this to get
// the file offset.
const armorSetBaseOffset = 0x0e50000

// ---------------------------------------------------------------------------
// On-disk structs (all little-endian)
//
// These mirror the layout documented in stingray/dl_bin/dl_bin.go so that we
// can parse arbitrary dl_bin blobs (not just the one embedded in that package).
// ---------------------------------------------------------------------------

type rawPiece struct {
	Path              stingray.Hash
	Slot              uint32
	PieceType         uint32
	Weight            uint32
	Unk00             uint32
	MaterialLut       stingray.Hash
	PatternLut        stingray.Hash
	CapeLut           stingray.Hash
	CapeGradient      stingray.Hash
	CapeNac           stingray.Hash
	DecalScalarFields stingray.Hash
	BaseData          stingray.Hash
	DecalSheet        stingray.Hash
	ToneVariations    stingray.Hash
}

type rawBody struct {
	BodyType      uint32
	Unk00         uint32
	PiecesAddress uint32 // 20-bit pointer (& 0xfffff) relative to armorSetBaseOffset
	Unk01         uint32
	PiecesCount   uint32
	Unk02         uint32
}

type rawKit struct {
	ID               uint32
	DlcID            uint32
	SetID            uint32
	NameUpper        uint32
	NameCased        uint32
	Description      uint32
	Rarity           uint32
	Passive          uint32
	Archive          stingray.Hash
	KitType          uint32
	Unk00            uint32
	BodyArrayAddress uint32 // 24-bit pointer (& 0xffffff) relative to armorSetBaseOffset
	Unk01            uint32
	BodyCount        uint32
	Unk02            uint32
}

type rawDlItem struct {
	Magic   [4]byte
	Unk00   uint32
	Unk01   uint32
	KitSize uint32 // byte-length of the Kit payload that follows (may be > binary.Size(rawKit))
	Unk02   uint32
	Unk03   uint32
	Kit     rawKit
}

// ---------------------------------------------------------------------------
// Name helpers
// ---------------------------------------------------------------------------

func slotName(v uint32) string {
	names := [...]string{
		"helmet", "cape", "torso", "hips",
		"left_leg", "right_leg", "left_arm", "right_arm",
		"left_shoulder", "right_shoulder",
	}
	if int(v) < len(names) {
		return names[v]
	}
	return fmt.Sprintf("unknown_%d", v)
}

func pieceTypeName(v uint32) string {
	switch v {
	case 0:
		return "armor"
	case 1:
		return "undergarment"
	case 2:
		return "accessory"
	}
	return fmt.Sprintf("unknown_%d", v)
}

func weightName(v uint32) string {
	switch v {
	case 0:
		return "light"
	case 1:
		return "medium"
	case 2:
		return "heavy"
	}
	return fmt.Sprintf("unknown_%d", v)
}

func bodyTypeName(v uint32) string {
	switch v {
	case 0:
		return "stocky"
	case 1:
		return "slim"
	case 2:
		return "unknown"
	case 3:
		return "any"
	}
	return fmt.Sprintf("unknown_%d", v)
}

func kitTypeName(v uint32) string {
	switch v {
	case 0:
		return "Armor"
	case 1:
		return "Helmet"
	case 2:
		return "Cape"
	}
	return fmt.Sprintf("unknown_%d", v)
}

func rarityName(v uint32) string {
	switch v {
	case 0:
		return "Common"
	case 1:
		return "Uncommon"
	case 2:
		return "Heroic"
	}
	return fmt.Sprintf("unknown_%d", v)
}

func passiveName(v uint32) string {
	switch v {
	case 0:
		return "None"
	case 1:
		return "Extra Padding"
	case 2:
		return "Scout"
	case 3:
		return "Fortified"
	case 4:
		return "Unknown_4"
	case 5:
		return "Electrical Conduit"
	case 6:
		return "Engineering Kit"
	case 7:
		return "Med-Kit"
	case 8:
		return "Servo-Assisted"
	case 9:
		return "Democracy Protects"
	case 10:
		return "Reinforced Epaulettes"
	case 11:
		return "Inflammable"
	case 12:
		return "Peak Physique"
	case 13:
		return "Advanced Filtration"
	case 14:
		return "Unflinching"
	case 15:
		return "Acclimated"
	case 16:
		return "Siege-Ready"
	case 17:
		return "Integrated Explosives"
	case 18:
		return "Gunslinger"
	case 19:
		return "Adreno-Defibrillator"
	case 20:
		return "Ballistic Padding"
	case 30:
		return "Feet First"
	}
	return fmt.Sprintf("unknown_%d", v)
}

// ---------------------------------------------------------------------------
// JSON output types
// ---------------------------------------------------------------------------

// JSONPiece describes a single armour piece within an armour set.
type JSONPiece struct {
	Path              string `json:"path_hash"`
	Slot              string `json:"slot"`
	PieceType         string `json:"piece_type"`
	Weight            string `json:"weight"`
	BodyType          string `json:"body_type"`
	MaterialLut       string `json:"material_lut"`
	PatternLut        string `json:"pattern_lut"`
	CapeLut           string `json:"cape_lut"`
	CapeGradient      string `json:"cape_gradient"`
	CapeNac           string `json:"cape_nac"`
	DecalScalarFields string `json:"decal_scalar_fields"`
	BaseData          string `json:"base_data"`
	DecalSheet        string `json:"decal_sheet"`
	ToneVariations    string `json:"tone_variations"`
}

// JSONArmorSet describes one armour set / customization kit.
type JSONArmorSet struct {
	ArchiveHash string      `json:"archive_hash"`
	Name        string      `json:"name"`
	SetID       uint32      `json:"set_id"`
	KitID       uint32      `json:"kit_id"`
	DlcID       uint32      `json:"dlc_id"`
	Rarity      string      `json:"rarity"`
	Passive     string      `json:"passive"`
	KitType     string      `json:"kit_type"`
	Pieces      []JSONPiece `json:"pieces"`
}

// JSONOutput is the top-level JSON document produced by ExtractDlBinJSON.
type JSONOutput struct {
	ArmorSets []JSONArmorSet `json:"armor_sets"`
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

// ParseDlBin parses a raw dl_bin byte slice.
// languageMap (uint32 → string) is used to resolve name IDs to human-readable
// text; pass nil to print raw hex IDs instead.
func ParseDlBin(data []byte, languageMap map[uint32]string) (*JSONOutput, error) {
	r := bytes.NewReader(data)

	getName := func(id uint32) string {
		if s, ok := languageMap[id]; ok {
			return s
		}
		return fmt.Sprintf("0x%08x", id)
	}

	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, fmt.Errorf("read item count: %w", err)
	}

	out := &JSONOutput{
		ArmorSets: make([]JSONArmorSet, 0, int(count)),
	}

	offset := int64(4) // bytes consumed so far (the count field)

	for i := uint32(0); i < count; i++ {
		if _, err := r.Seek(offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("item %d: seek to 0x%x: %w", i, offset, err)
		}

		var item rawDlItem
		if err := binary.Read(r, binary.LittleEndian, &item); err != nil {
			return nil, fmt.Errorf("item %d: read DlItem header: %w", i, err)
		}

		kit := item.Kit

		// ── Read bodies ──────────────────────────────────────────────────────
		bodyAddr := int64((kit.BodyArrayAddress&0xffffff) - armorSetBaseOffset)
		if bodyAddr < 0 || bodyAddr >= int64(len(data)) {
			return nil, fmt.Errorf("item %d: body array address 0x%x out of range", i, bodyAddr)
		}
		if _, err := r.Seek(bodyAddr, io.SeekStart); err != nil {
			return nil, fmt.Errorf("item %d: seek to body array: %w", i, err)
		}

		bodies := make([]rawBody, kit.BodyCount)
		if err := binary.Read(r, binary.LittleEndian, bodies); err != nil {
			return nil, fmt.Errorf("item %d: read %d bodies: %w", i, kit.BodyCount, err)
		}

		as := JSONArmorSet{
			ArchiveHash: kit.Archive.String(),
			Name:        getName(kit.NameCased),
			SetID:       kit.SetID,
			KitID:       kit.ID,
			DlcID:       kit.DlcID,
			Rarity:      rarityName(kit.Rarity),
			Passive:     passiveName(kit.Passive),
			KitType:     kitTypeName(kit.KitType),
			Pieces:      []JSONPiece{},
		}

		// ── Read pieces for each body ─────────────────────────────────────────
		for _, body := range bodies {
			piecesAddr := int64((body.PiecesAddress & 0xfffff) - armorSetBaseOffset)
			if piecesAddr < 0 || piecesAddr >= int64(len(data)) {
				return nil, fmt.Errorf("item %d: pieces address 0x%x out of range", i, piecesAddr)
			}
			if _, err := r.Seek(piecesAddr, io.SeekStart); err != nil {
				return nil, fmt.Errorf("item %d: seek to pieces: %w", i, err)
			}

			pieces := make([]rawPiece, body.PiecesCount)
			if err := binary.Read(r, binary.LittleEndian, pieces); err != nil {
				return nil, fmt.Errorf("item %d: read %d pieces: %w", i, body.PiecesCount, err)
			}

			for _, p := range pieces {
				as.Pieces = append(as.Pieces, JSONPiece{
					Path:              p.Path.String(),
					Slot:              slotName(p.Slot),
					PieceType:         pieceTypeName(p.PieceType),
					Weight:            weightName(p.Weight),
					BodyType:          bodyTypeName(body.BodyType),
					MaterialLut:       p.MaterialLut.String(),
					PatternLut:        p.PatternLut.String(),
					CapeLut:           p.CapeLut.String(),
					CapeGradient:      p.CapeGradient.String(),
					CapeNac:           p.CapeNac.String(),
					DecalScalarFields: p.DecalScalarFields.String(),
					BaseData:          p.BaseData.String(),
					DecalSheet:        p.DecalSheet.String(),
					ToneVariations:    p.ToneVariations.String(),
				})
			}
		}

		out.ArmorSets = append(out.ArmorSets, as)

		// ── Advance to next item ──────────────────────────────────────────────
		// KitSize is the byte-length of the Kit sub-struct in this specific
		// file version.  The fixed rawDlItem header is the part before Kit.
		headerSize := int64(binary.Size(item)) - int64(binary.Size(item.Kit))
		offset += headerSize + int64(item.KitSize)
	}

	return out, nil
}

// ---------------------------------------------------------------------------
// ExtractDlBinJSON – primary extractor (registered as "json" format)
// ---------------------------------------------------------------------------

// ExtractDlBinJSON reads a dl_bin game resource and writes its parsed content
// as a pretty-printed JSON file.  If parsing fails it falls back to a raw dump
// and emits a warning so that the user still gets their data after a game
// update that changes the binary format.
func ExtractDlBinJSON(ctx *extractor.Context) error {
	data, err := ctx.Read(ctx.FileID(), stingray.DataMain)
	if err != nil {
		return fmt.Errorf("dl_bin: read main data: %w", err)
	}

	out, parseErr := ParseDlBin(data, ctx.LanguageMap())
	if parseErr != nil {
		ctx.Warnf("JSON parse failed (%v); writing raw .dl_bin instead", parseErr)
		return writeRaw(ctx, data)
	}

	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("dl_bin: marshal JSON: %w", err)
	}

	f, err := ctx.CreateFile(".json")
	if err != nil {
		return fmt.Errorf("dl_bin: create JSON output file: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(encoded); err != nil {
		return fmt.Errorf("dl_bin: write JSON: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// ExtractDlBinRaw – verbatim dump (registered as "raw" format)
// ---------------------------------------------------------------------------

// ExtractDlBinRaw writes the main-data blob of a dl_bin resource verbatim as
// a .dl_bin file.  Use this when you want the binary for further inspection or
// when ExtractDlBinJSON cannot parse the file.
func ExtractDlBinRaw(ctx *extractor.Context) error {
	data, err := ctx.Read(ctx.FileID(), stingray.DataMain)
	if err != nil {
		return fmt.Errorf("dl_bin: read main data: %w", err)
	}
	return writeRaw(ctx, data)
}

func writeRaw(ctx *extractor.Context, data []byte) error {
	f, err := ctx.CreateFile(".dl_bin")
	if err != nil {
		return fmt.Errorf("dl_bin: create raw output file: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("dl_bin: write raw data: %w", err)
	}
	return nil
}
