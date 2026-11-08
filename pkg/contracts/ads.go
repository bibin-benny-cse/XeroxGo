package contracts

import "time"

type AdType string

const (
	AdTypeGradient AdType = "gradient"
	AdTypeImage    AdType = "image"
)

type AdItem struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Type        AdType    `json:"type"`
	ImageURL    string    `json:"image_url,omitempty"`
	GradientC0  string    `json:"gradient_c0"` // #RRGGBB
	GradientC1  string    `json:"gradient_c1"`
	GradientC2  string    `json:"gradient_c2"`
	LinkURL     string    `json:"link_url,omitempty"`
	SortOrder   int       `json:"sort_order"`
	IsActive    bool      `json:"is_active"`
	UpdatedAt   time.Time `json:"updated_at"`
}
