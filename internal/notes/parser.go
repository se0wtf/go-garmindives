package notes

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

var (
	diveNoteHeader = regexp.MustCompile(`(?i)^(n|commentaires?|hl)[\s\p{Zs}]*:[\s\p{Zs}]*(.*)$`)
	diveNoteRating = regexp.MustCompile(`(?i)^n\s*[:=]?\s*([0-9]+(?:[.,][0-9]+)?)\s*(?:/\s*([0-9]+(?:[.,][0-9]+)?))?\s*$`)
)

type Parsed struct {
	Rating                               *float64
	RatingMax                            *float64
	Comment                              string
	Highlights                           []string
	hasRating, hasComment, hasHighlights bool
}

// Legacy Dive notes take precedence per section. The official Personal API
// carries the activity's free-text note in description. Explicitly empty
// sections in notes remain empty instead of inheriting another source's text.
func Parse(notes, description string) Parsed {
	result := parseDiveNote(notes)
	fallback := parseDiveNote(description)
	if !result.hasRating {
		result.Rating, result.RatingMax = fallback.Rating, fallback.RatingMax
	}
	if !result.hasComment {
		result.Comment = fallback.Comment
	}
	if !result.hasHighlights {
		result.Highlights = fallback.Highlights
	}
	return result
}

func parseDiveNote(text string) Parsed {
	result := Parsed{Highlights: []string{}}
	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var comments []string
	section := ""
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		// Nonbreaking spaces often appear in text copied from a French keyboard.
		headerText := strings.ReplaceAll(strings.ReplaceAll(trimmed, "\u00a0", " "), "\u202f", " ")
		if rating := diveNoteRating.FindStringSubmatch(headerText); rating != nil {
			result.hasRating = true
			result.Rating, result.RatingMax = parseDiveRating(rating[1], rating[2])
			section = ""
			continue
		}
		if header := diveNoteHeader.FindStringSubmatch(trimmed); header != nil {
			switch strings.ToLower(header[1]) {
			case "n":
				// A malformed explicit rating is unavailable, never zero or a
				// rating from the other source. Keep its raw text in notes.
				result.hasRating, result.Rating, result.RatingMax = true, nil, nil
				section = ""
			case "commentaire", "commentaires":
				result.hasComment, section = true, "comment"
				if len(comments) > 0 {
					comments = append(comments, "")
				}
				if value := strings.TrimSpace(header[2]); value != "" {
					comments = append(comments, value)
				}
			case "hl":
				result.hasHighlights, section = true, "highlights"
				if value := diveHighlight(header[2]); value != "" {
					result.Highlights = append(result.Highlights, value)
				}
			}
			continue
		}
		switch section {
		case "comment":
			comments = append(comments, line)
		case "highlights":
			if value := diveHighlight(line); value != "" {
				result.Highlights = append(result.Highlights, value)
			}
		}
	}
	result.Comment = strings.TrimSpace(strings.Join(comments, "\n"))
	return result
}

func parseDiveRating(score, maximum string) (*float64, *float64) {
	value, err := strconv.ParseFloat(strings.ReplaceAll(score, ",", "."), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return nil, nil
	}
	if maximum == "" {
		return &value, nil
	}
	limit, err := strconv.ParseFloat(strings.ReplaceAll(maximum, ",", "."), 64)
	if err != nil || math.IsNaN(limit) || math.IsInf(limit, 0) || limit <= 0 || value > limit {
		return nil, nil
	}
	return &value, &limit
}

func diveHighlight(line string) string {
	line = strings.TrimSpace(line)
	switch line {
	case "-", "*", "•", "–":
		return ""
	}
	for _, bullet := range []string{"- ", "* ", "• ", "– "} {
		if strings.HasPrefix(line, bullet) {
			return strings.TrimSpace(strings.TrimPrefix(line, bullet))
		}
	}
	return line
}
