package opentsdb

type otdbQueryResSet []*otdbQueryRes

type otdbQueryRes struct {
	Metric TagValue `json:"metric"`
	// A list of tags only returned when the results are for a single time series.
	// If results are aggregated, this value may be null or an empty map
	Tags map[string]TagValue `json:"tags"`
	// If more than one timeseries were included in the result set, i.e. they were
	// aggregated, this will display a list of tag names that were found in common across all time series.
	AggregatedTags map[string]TagValue `json:"aggregatedTags"`
	DPs            otdbDPs             `json:"dps"`
}

type otdbDPs map[int64]float64

type otdbQueryReq struct {
	Start   int64       `json:"start"`
	End     int64       `json:"end"`
	Queries []otdbQuery `json:"queries"`
}

type otdbQuery struct {
	Metric     TagValue     `json:"metric"`
	Filters    []otdbFilter `json:"filters"`
	Aggregator string       `json:"aggregator"`
}

type otdbFilterType string

const (
	otdbFilterTypeLiteralOr    = "literal_or"
	otdbFilterTypeNotLiteralOr = "not_literal_or"
)

type otdbFilter struct {
	Type    otdbFilterType `json:"type"`
	Tagk    string         `json:"tagk"`
	Filter  string         `json:"filter"`
	GroupBy bool           `json:"groupBy"`
}
