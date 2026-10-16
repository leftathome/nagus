package promo

import (
	"regexp"

	"github.com/leftathome/nagus/internal/deal"
)

// A retailer newsletter mixes categories: a Newegg mail is mostly laptops,
// monitors and GPUs. The category extractors downstream judge what reaches
// them (the hdd extractor refuses SSDs, the wine extractor non-wine), but the
// hdd extractor was written for drive-only feeds and does not refuse a
// laptop. So a promo product for hdd must also LOOK like a hard drive before
// it is emitted: a capacity, a drive word, and no computer word. Drop-only:
// it can never admit what the extractor would refuse.
//
// Computer words come in two strengths. "desktop", "pc", "computer" and
// "tower" are also how drives are sold ("WD Blue 4TB Desktop Hard Disk
// Drive", "...for Computer Desktop PC"), so they refuse a title only when it
// has no strong drive phrase (hard drive, hard disk drive, HDD). The rest
// always refuse, and so do computer SPECS (RAM, a CPU, an OS, drive bays, a
// NAS box) and the phrases "desktop pc", "desktop computer", "tower
// desktop": a prebuilt PC or a NAS lists its drive too ("HP Desktop PC 1TB
// HDD 16GB RAM", "Synology DS224+ 2-Bay NAS 8TB HDD").
//
// Computer SPECS (and PC and NAS product lines) are read on the full title.
// Everything else is read with a trailing purpose clause removed: "...
// Internal Hard Drive 7200 RPM for Computer Desktop PC" says what the drive
// is FOR, not what it is.
var (
	hddCapacity = regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s*(?:TB|GB)\b`)
	hddWord     = regexp.MustCompile(`(?i)\b(?:hdd|hard\s*(?:disk\s*)?drive|harddrive|[0-9]{4,5}\s*rpm|nas\s+drive|sata|sas|cmr|smr|ironwolf|exos|barracuda|ultrastar|skyhawk|wd\s*(?:red|purple|gold|blue|black)|n300|x300|mg[0-9]{2})\b`)
	hddStrong   = regexp.MustCompile(`(?i)\b(?:hdd|hard\s*(?:disk\s*)?drive|harddrive)\b`)
	hddNotDrive = regexp.MustCompile(`(?i)\b(?:laptop|notebook|chromebook|tablet|monitor|all-in-one|motherboard|graphics\s+card|gpu|keyboard|mouse|router|headset|console|enclosure|docking|dock|nas\s+server|gaming\s+(?:desktop|pc|laptop))\b`)
	hddWeakPC   = regexp.MustCompile(`(?i)\b(?:desktop|pc|computer|tower)\b`)
	// hddPCSpec is read on the FULL title: a spec in a trailing clause
	// ("... 1TB HDD for Gaming, Ryzen 7, 32GB DDR5") is still a computer's.
	hddPCSpec = regexp.MustCompile(`(?i)\b(?:\d+\s*GB\s*(?:DDR\d|RAM|memory)|core\s*i[3579]|ryzen|windows\s*1[01]|synology|qnap|` +
		`inspiron|optiplex|vostro|ideacentre|thinkcentre|pavilion|omen|aspire|expertcenter|imac|mac\s*mini|` +
		`linkstation|terastation|my\s*cloud|diskstation|personal\s+cloud|` +
		`mini\s*pc|xeon|veriton|precision|workstation|asustor|drivestor|` +
		`intel\s+(?:core|xeon|celeron|pentium|n[0-9]{2,3}|i[3579]))\b`)
	// hddPCPhrase is read on the title WITHOUT its purpose clause: "for
	// Computer Desktop PC" or "for 8-Bay NAS" says what a drive is for.
	hddPCPhrase = regexp.MustCompile(`(?i)\b(?:desktop\s+(?:pc|computer)|tower\s+desktop|\d+-bay)\b`)
	// A NAS DRIVE is a drive; any other "NAS" is a box.
	nasDrive = regexp.MustCompile(`(?i)\bnas\s+(?:internal\s+)?(?:hard\s+)?(?:disk\s+)?drive\b`)
	nasWord  = regexp.MustCompile(`(?i)\bnas\b`)
	purpose  = regexp.MustCompile(`(?i)\s+for\s+(?:\d+(?:-\d+)?\s*-?\s*bay\s+)?(?:computer|desktop|pc|laptop|gaming|nas|servers?|surveillance)\b.*$`)
)

// categoryFit reports whether a product title can belong to category before
// the extractor sees it.
func categoryFit(category, title string) bool {
	if category != deal.CategoryHDD {
		return true // the wine extractor's own non-wine rules decide
	}
	if hddPCSpec.MatchString(title) {
		return false
	}
	t := purpose.ReplaceAllString(title, "")
	if !hddCapacity.MatchString(t) || !hddWord.MatchString(t) || hddNotDrive.MatchString(t) || hddPCPhrase.MatchString(t) {
		return false
	}
	if nasWord.MatchString(nasDrive.ReplaceAllString(t, "")) {
		return false
	}
	return hddStrong.MatchString(t) || !hddWeakPC.MatchString(t)
}
