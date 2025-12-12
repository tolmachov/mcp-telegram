package messages

import "testing"

func TestExtractSubstring(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		offset int
		length int
		want   string
	}{
		{
			name:   "ASCII text",
			input:  "Hello, World!",
			offset: 7,
			length: 5,
			want:   "World",
		},
		{
			name:   "ASCII from start",
			input:  "Hello",
			offset: 0,
			length: 5,
			want:   "Hello",
		},
		{
			name:   "Cyrillic text",
			input:  "Привет, мир!",
			offset: 8,
			length: 3,
			want:   "мир",
		},
		{
			name:   "Cyrillic from start",
			input:  "Привет",
			offset: 0,
			length: 6,
			want:   "Привет",
		},
		{
			name:   "Emoji (surrogate pair)",
			input:  "Hello 👋 World",
			offset: 6,
			length: 2, // emoji takes 2 code units
			want:   "👋",
		},
		{
			name:   "Text after emoji",
			input:  "Hello 👋 World",
			offset: 9, // 6 + 2 (emoji) + 1 (space)
			length: 5,
			want:   "World",
		},
		{
			name:   "Multiple emojis",
			input:  "🎉🎊🎁",
			offset: 2, // after first emoji
			length: 2, // second emoji
			want:   "🎊",
		},
		{
			name:   "Mixed: Cyrillic and emoji",
			input:  "Привет 👋",
			offset: 0,
			length: 6,
			want:   "Привет",
		},
		{
			name:   "Mixed: emoji in Cyrillic",
			input:  "Привет 👋 мир",
			offset: 7,
			length: 2,
			want:   "👋",
		},
		{
			name:   "URL in text",
			input:  "Check https://example.com for info",
			offset: 6,
			length: 19,
			want:   "https://example.com",
		},
		{
			name:   "URL after Cyrillic",
			input:  "Ссылка: https://example.com",
			offset: 8,
			length: 19,
			want:   "https://example.com",
		},
		{
			name:   "Empty string",
			input:  "",
			offset: 0,
			length: 1,
			want:   "",
		},
		{
			name:   "Negative offset",
			input:  "Hello",
			offset: -1,
			length: 3,
			want:   "",
		},
		{
			name:   "Zero length",
			input:  "Hello",
			offset: 0,
			length: 0,
			want:   "",
		},
		{
			name:   "Negative length",
			input:  "Hello",
			offset: 0,
			length: -1,
			want:   "",
		},
		{
			name:   "Offset beyond string",
			input:  "Hello",
			offset: 10,
			length: 1,
			want:   "",
		},
		{
			name:   "Length exceeds string",
			input:  "Hello",
			offset: 0,
			length: 100,
			want:   "",
		},
		{
			name:   "Single character",
			input:  "A",
			offset: 0,
			length: 1,
			want:   "A",
		},
		{
			name:   "Single Cyrillic character",
			input:  "Я",
			offset: 0,
			length: 1,
			want:   "Я",
		},
		{
			name:   "Single emoji",
			input:  "🔥",
			offset: 0,
			length: 2,
			want:   "🔥",
		},
		{
			name:   "Flag emoji (ZWJ sequence)",
			input:  "Hi 🇺🇸 there",
			offset: 3,
			length: 4, // flag emoji: 2 regional indicators × 2 code units each
			want:   "🇺🇸",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSubstring(tt.input, tt.offset, tt.length)
			if got != tt.want {
				t.Errorf("extractSubstring(%q, %d, %d) = %q, want %q",
					tt.input, tt.offset, tt.length, got, tt.want)
			}
		})
	}
}
