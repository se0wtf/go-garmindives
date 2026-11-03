package app

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

const maximumRatingFilters = 100

func parseRatings(values []string) ([]float64, error) {
	var ratings []float64
	seen := make(map[float64]bool)
	count := 0
	for _, raw := range values {
		for _, value := range strings.Split(raw, ",") {
			count++
			if count > maximumRatingFilters {
				return nil, errors.New("ratings must contain at most 100 values")
			}
			rating, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || math.IsNaN(rating) || math.IsInf(rating, 0) || rating < 0 {
				return nil, errors.New("ratings must contain finite nonnegative numbers separated by commas (for example, ratings=4,5)")
			}
			if !seen[rating] {
				ratings = append(ratings, rating)
				seen[rating] = true
			}
		}
	}
	return ratings, nil
}
