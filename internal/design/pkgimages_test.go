package design

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestImageIndexMatches(t *testing.T) {
	x := newImageIndex([]string{
		"assets/icons/heart.svg",
		"assets/icons/heart_medium.svg",
		"assets/icons/heart_rate.svg",
		"assets/icons/linkedin.svg",
		"assets/images/acme-logo.png",
		"assets/icons/chevron_down.svg",
		"assets/icons/alt/heart.svg",
	})

	tests := []struct {
		name string
		want []string
	}{
		{
			name: "heart",
			want: []string{
				"assets/icons/heart.svg",
				"assets/icons/alt/heart.svg",
				"assets/icons/heart_rate.svg",
				"assets/icons/heart_medium.svg",
			},
		},
		{name: "Logo", want: []string{"assets/images/acme-logo.png"}},
		{name: "ChevronDown", want: []string{"assets/icons/chevron_down.svg"}},
		{name: "link"},
		{name: "_"},
		{name: "rate", want: []string{"assets/icons/heart_rate.svg"}},
		{name: "down_chevron"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, x.matches(tt.name))
		})
	}
}

func TestImageIndexWords(t *testing.T) {
	x := newImageIndex([]string{"assets/icons/ChevronDown.svg", "a/b.c/acme-logo.png"})

	assert.Equal(t, [][]string{{"chevron", "down"}, {"acme", "logo"}}, x.words)
}

func TestPickImages(t *testing.T) {
	tests := []struct {
		name   string
		ranked [][]string
		limit  int
		want   []string
	}{
		{
			name:   "rounds across lists",
			ranked: [][]string{{"a1", "a2", "a3"}, {"b1"}},
			limit:  3,
			want:   []string{"a1", "b1", "a2"},
		},
		{
			name:   "a shared path once",
			ranked: [][]string{{"a1", "a2"}, {"a1", "b2"}},
			limit:  10,
			want:   []string{"a1", "a2", "b2"},
		},
		{
			name:   "limit inside a list",
			ranked: [][]string{{"a1", "a2", "a3"}},
			limit:  2,
			want:   []string{"a1", "a2"},
		},
		{
			name:   "limit inside a round",
			ranked: [][]string{{"a1"}, {"b1"}, {"c1"}},
			limit:  2,
			want:   []string{"a1", "b1"},
		},
		{name: "no lists", ranked: nil, limit: 5},
		{name: "zero limit", ranked: [][]string{{"a1"}}, limit: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pickImages(tt.ranked, tt.limit))
		})
	}
}
