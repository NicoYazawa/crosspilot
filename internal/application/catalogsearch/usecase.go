// Package catalogsearch 是商品检索的用例服务.
package catalogsearch

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/catalog/ports"
	"github.com/NicoYazawa/crosspilot/internal/domain/shipping"
)

const (
	recallCandidates = 32
	filteredOutLimit = 3
)

type CatalogSearchUseCase struct {
	productRepo    ports.ProductRepository
	embedder       ports.EmbeddingClient
	vectorIndex    ports.ProductVectorIndex
	reranker       ports.Reranker
	tariffSchedule *shipping.TariffSchedule
}

func New(
	productRepo ports.ProductRepository,
	embedder ports.EmbeddingClient,
	vectorIndex ports.ProductVectorIndex,
	reranker ports.Reranker,
	tariffSchedule *shipping.TariffSchedule,
) *CatalogSearchUseCase {
	if tariffSchedule == nil {
		tariffSchedule = shipping.NewTariffSchedule(nil)
	}
	return &CatalogSearchUseCase{
		productRepo:    productRepo,
		embedder:       embedder,
		vectorIndex:    vectorIndex,
		reranker:       reranker,
		tariffSchedule: tariffSchedule,
	}
}

func (uc *CatalogSearchUseCase) Execute(ctx context.Context, spec catalog.ProductSearchSpec) (SearchResult, error) {
	if err := spec.Validate(); err != nil {
		return SearchResult{}, err
	}
	identifiers := extractIdentifiers(spec)
	if len(identifiers) > 0 {
		return uc.executeExactIDs(ctx, spec, identifiers)
	}
	return uc.executeSearch(ctx, spec)
}

func (uc *CatalogSearchUseCase) executeSearch(ctx context.Context, spec catalog.ProductSearchSpec) (SearchResult, error) {
	var scored []ScoredProduct
	var recallStrategy string

	if uc.embedder != nil && uc.vectorIndex != nil {
		embedding, err := uc.embedder.Embed(ctx, spec.NormalizedQuery)
		if err != nil {
			scored, recallStrategy = uc.keywordRecall(ctx, spec)
		} else {
			topN := max(recallCandidates, spec.TopK*4)
			hits, err := uc.vectorIndex.Search(ctx, embedding, topN)
			if err != nil {
				scored, recallStrategy = uc.keywordRecall(ctx, spec)
			} else {
				productIDs := make([]string, len(hits))
				for i, hit := range hits {
					productIDs[i] = hit.ProductID
				}
				products, err := uc.productRepo.FindByIDs(ctx, productIDs)
				if err != nil {
					return SearchResult{}, err
				}
				byID := make(map[string]catalog.Product, len(products))
				for _, p := range products {
					byID[p.ID] = p
				}
				for _, hit := range hits {
					if p, ok := byID[hit.ProductID]; ok {
						scored = append(scored, ScoredProduct{Score: hit.Score, Product: p})
					}
				}
				recallStrategy = "embedding_only"
			}
		}
	} else {
		scored, recallStrategy = uc.keywordRecall(ctx, spec)
	}

	if recallStrategy == "embedding_only" && len(scored) > 0 && uc.reranker != nil {
		reranked, err := uc.rerank(ctx, spec.NormalizedQuery, scored)
		if err == nil {
			scored = reranked
			recallStrategy = "embedding_rerank"
		}
	}

	filtered, filteredOut := uc.applyFilters(scored, spec)

	hits := make([]ProductCard, 0, min(len(filtered), spec.TopK))
	for i := 0; i < min(len(filtered), spec.TopK); i++ {
		hits = append(hits, uc.toCard(filtered[i].Score, filtered[i].Product, spec))
	}

	return SearchResult{
		Hits:            hits,
		TotalCandidates: len(filtered),
		RecallStrategy:  recallStrategy,
		RerankApplied:   recallStrategy == "embedding_rerank",
		FilteredOut:     filteredOut,
	}, nil
}

func (uc *CatalogSearchUseCase) executeExactIDs(ctx context.Context, spec catalog.ProductSearchSpec, identifiers []string) (SearchResult, error) {
	productIDs := make([]string, 0, len(identifiers))
	seen := make(map[string]bool)
	for _, id := range identifiers {
		productID := id
		if idx := strings.Index(id, "-S"); idx != -1 {
			productID = id[:idx]
		}
		if productID != "" && !seen[productID] {
			seen[productID] = true
			productIDs = append(productIDs, productID)
		}
	}

	products, err := uc.productRepo.FindByIDs(ctx, productIDs)
	if err != nil {
		return SearchResult{}, err
	}
	byID := make(map[string]catalog.Product, len(products))
	for _, p := range products {
		byID[p.ID] = p
	}

	var hits []ProductCard
	var rejected []FilteredOut
	var missing []string

	for _, productID := range productIDs {
		product, ok := byID[productID]
		if !ok {
			missing = append(missing, productID)
			continue
		}

		primary := product.PrimaryAvailableSKU()
		reason := uc.rejectReason(product, spec, primary)
		if reason != "" {
			rejected = append(rejected, uc.toFilteredOut(product, spec, reason, primary))
			continue
		}

		hits = append(hits, uc.toCardWithSKU(1.0, product, spec, primary))
	}

	result := SearchResult{
		Hits:               hits,
		TotalCandidates:    len(hits),
		RecallStrategy:     "exact_id_lookup",
		RerankApplied:      false,
		FilteredOut:        rejected,
		MissingIdentifiers: missing,
		ExistenceChecked:   true,
	}

	if len(hits) > spec.TopK {
		result.Hits = result.Hits[:spec.TopK]
	}
	if len(rejected) > filteredOutLimit {
		result.FilteredOut = result.FilteredOut[:filteredOutLimit]
	}

	return result, nil
}

func (uc *CatalogSearchUseCase) rerank(ctx context.Context, query string, scored []ScoredProduct) ([]ScoredProduct, error) {
	if uc.reranker == nil {
		return nil, errRerankerNotConfigured
	}

	products := make([]catalog.Product, len(scored))
	for i, s := range scored {
		products[i] = s.Product
	}

	rerankScores, err := uc.reranker.Rerank(ctx, query, products)
	if err != nil {
		return nil, err
	}

	if len(rerankScores) != len(scored) {
		return nil, errRerankScoreLengthMismatch
	}

	reranked := make([]ScoredProduct, len(scored))
	for i, s := range scored {
		reranked[i] = ScoredProduct{Score: rerankScores[i], Product: s.Product}
	}

	slices.SortFunc(reranked, func(a, b ScoredProduct) int {
		if b.Score == a.Score {
			return strings.Compare(a.Product.ID, b.Product.ID)
		}
		if b.Score > a.Score {
			return 1
		}
		return -1
	})

	return reranked, nil
}

func (uc *CatalogSearchUseCase) keywordRecall(ctx context.Context, spec catalog.ProductSearchSpec) ([]ScoredProduct, string) {
	queryTerms := tokenize(spec.NormalizedQuery)
	allProducts, err := uc.productRepo.ListAll(ctx)
	if err != nil {
		return nil, "keyword_2gram"
	}

	var candidates []ScoredProduct
	for _, product := range allProducts {
		score := keywordScore(queryTerms, product, spec)
		if score > 0 {
			candidates = append(candidates, ScoredProduct{Score: float32(score), Product: product})
		}
	}

	slices.SortFunc(candidates, func(a, b ScoredProduct) int {
		if b.Score == a.Score {
			return strings.Compare(a.Product.ID, b.Product.ID)
		}
		if b.Score > a.Score {
			return 1
		}
		return -1
	})

	return candidates, "keyword_2gram"
}

func (uc *CatalogSearchUseCase) applyFilters(scored []ScoredProduct, spec catalog.ProductSearchSpec) ([]ScoredProduct, []FilteredOut) {
	var filtered []ScoredProduct
	var filteredOut []FilteredOut

	for _, s := range scored {
		primary := s.Product.PrimaryAvailableSKU()
		reason := uc.rejectReason(s.Product, spec, primary)
		if reason == "" {
			filtered = append(filtered, s)
		} else if len(filteredOut) < filteredOutLimit {
			filteredOut = append(filteredOut, uc.toFilteredOut(s.Product, spec, reason, primary))
		}
	}

	return filtered, filteredOut
}

func (uc *CatalogSearchUseCase) rejectReason(product catalog.Product, spec catalog.ProductSearchSpec, primary catalog.SKU) string {
	if primary.ID != "" && primary.Stock <= 0 {
		return "requested_sku_out_of_stock"
	}
	if !product.HasAvailableSKU() {
		return "out_of_stock"
	}
	if spec.Category != "" && product.Category != spec.Category {
		return "category_mismatch"
	}
	if len(spec.ExcludedMaterialTags) > 0 {
		for _, tag := range spec.ExcludedMaterialTags {
			if slices.Contains(product.MaterialTags, tag) {
				return "material_excluded"
			}
		}
	}
	if len(spec.RequiredMaterialTags) > 0 {
		for _, required := range spec.RequiredMaterialTags {
			if !slices.Contains(product.MaterialTags, required) {
				return "material_required_missing"
			}
		}
	}
	if spec.ShipTo != "" {
		if !slices.Contains(product.ShipsTo, spec.ShipTo) {
			return "ship_to_unavailable"
		}
	}
	if !uc.withinPriceCap(product, spec, primary) {
		return "over_price_cap"
	}
	return ""
}

func (uc *CatalogSearchUseCase) withinPriceCap(product catalog.Product, spec catalog.ProductSearchSpec, primary catalog.SKU) bool {
	if spec.PriceMaxMajor == nil {
		return true
	}
	price := product.Price()
	if primary.ID != "" {
		price = primary.Price
	}
	return price.ToMajorUnitsFloat() <= *spec.PriceMaxMajor
}

func (uc *CatalogSearchUseCase) toFilteredOut(product catalog.Product, spec catalog.ProductSearchSpec, reason string, primary catalog.SKU) FilteredOut {
	price := product.Price()
	if primary.ID != "" {
		price = primary.Price
	}
	return FilteredOut{
		ProductID:  product.ID,
		Title:      product.Title,
		Category:   product.Category,
		PriceMajor: price.ToMajorUnitsFloat(),
		Currency:   catalog.Currency(spec.TargetCurrency),
		Reason:     reason,
	}
}

func (uc *CatalogSearchUseCase) toCard(score float32, product catalog.Product, spec catalog.ProductSearchSpec) ProductCard {
	primary := product.PrimaryAvailableSKU()
	return uc.toCardWithSKU(score, product, spec, primary)
}

func (uc *CatalogSearchUseCase) toCardWithSKU(score float32, product catalog.Product, spec catalog.ProductSearchSpec, primary catalog.SKU) ProductCard {
	price := product.Price()
	if primary.ID != "" {
		price = primary.Price
	}

	highlights := make([]string, 0, len(product.Highlights))
	for _, h := range product.Highlights {
		highlights = append(highlights, h)
	}

	skus := make([]catalog.SKUInfo, len(product.SKUs))
	for i, s := range product.SKUs {
		skus[i] = s.ToSKUInfo()
	}

	var landedPrice *LandedPriceInfo
	if spec.ShipTo != "" {
		quote, err := uc.tariffSchedule.Quote(price, product.Category, spec.ShipTo, 1, spec.TargetCurrency)
		if err != nil {
			landedPrice = &LandedPriceInfo{UnavailableReason: err.Error()}
		} else {
			landed, _ := quote.LandedTotal()
			landedPrice = &LandedPriceInfo{
				SubtotalMajor: quote.Subtotal.ToMajorUnitsFloat(),
				ShippingMajor: quote.Freight.ToMajorUnitsFloat(),
				TariffMajor:   quote.Tariff.ToMajorUnitsFloat(),
				TotalMajor:    landed.ToMajorUnitsFloat(),
				Currency:      string(quote.Subtotal.Currency),
			}
		}
	}

	return ProductCard{
		ProductID:          product.ID,
		Title:              product.Title,
		Brand:              product.Brand,
		Category:           product.Category,
		OriginCountry:      product.OriginCountry,
		PriceMajor:         price.ToMajorUnitsFloat(),
		Currency:           string(spec.TargetCurrency),
		SourcePriceMajor:   price.ToMajorUnitsFloat(),
		SourceCurrency:     string(price.Currency),
		Highlights:         highlights,
		SKUs:               skus,
		Score:              score,
		LandedPrice:        landedPrice,
		SourcePlatform:     product.SourcePlatform,
		CanonicalProductID: product.CanonicalID,
		MaterialTags:       product.MaterialTags,
		WeightKg:           product.WeightKg,
		Description:        product.Description,
		RatingSummary:      product.RatingSummary,
		RatingIsLive:       product.RatingIsLive,
		ShipsTo:            product.ShipsTo,
		DimensionsCm:       product.DimensionsCm,
		UpdatedAt:          product.UpdatedAt,
		DefaultSKUID:       primary.ID,
		ImageURL:           product.ImageURL,
		ImageKind:          product.ImageKind,
		ImageAlt:           product.ImageAlt,
		SourceLanguage:     product.SourceLanguage,
		SourceLocale:       product.SourceLocale,
		DataProvenance:     product.DataProvenance,
	}
}

func extractIdentifiers(spec catalog.ProductSearchSpec) []string {
	var identifiers []string

	if spec.ProductID != "" {
		identifiers = append(identifiers, spec.ProductID)
	}
	if spec.SkuID != "" {
		identifiers = append(identifiers, spec.SkuID)
	}

	re := regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])P\d{4,}(?:-S\d+)?(?:$|[^A-Za-z0-9-])`)
	matches := re.FindAllString(spec.NormalizedQuery, -1)
	seen := make(map[string]bool)
	for _, m := range matches {
		trimmed := strings.TrimSpace(m)
		upper := strings.ToUpper(trimmed)
		if !seen[upper] {
			seen[upper] = true
			identifiers = append(identifiers, upper)
		}
	}

	return identifiers
}

func tokenize(text string) []string {
	terms := make([]string, 0, 16)
	seen := make(map[string]bool)

	for _, chunk := range strings.Fields(strings.ToLower(text)) {
		if !seen[chunk] {
			seen[chunk] = true
			terms = append(terms, chunk)
		}
		if hasCJK(chunk) && len(chunk) >= 2 {
			runes := []rune(chunk)
			for i := 0; i < len(runes)-1; i++ {
				gram := string(runes[i : i+2])
				if !seen[gram] {
					seen[gram] = true
					terms = append(terms, gram)
				}
			}
		}
	}

	return terms
}

func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}

func keywordScore(queryTerms []string, product catalog.Product, spec catalog.ProductSearchSpec) float64 {
	docTerms := tokenize(product.SearchableText())
	matched := 0
	for _, qt := range queryTerms {
		for _, dt := range docTerms {
			if qt == dt {
				matched++
				break
			}
		}
	}
	if matched == 0 {
		return 0
	}
	score := float64(matched)
	if spec.Category != "" && strings.Contains(product.Category, spec.Category) {
		score += 3.0
	}
	return score
}

type ScoredProduct struct {
	Score   float32
	Product catalog.Product
}

type SearchResult struct {
	Hits                 []ProductCard
	TotalCandidates      int
	RecallStrategy       string
	RerankApplied        bool
	FilteredOut          []FilteredOut
	MissingIdentifiers   []string
	RequestedIdentifiers []string
	ExistenceChecked     bool
}

type FilteredOut struct {
	ProductID  string           `json:"product_id"`
	Title      string           `json:"title"`
	Category   string           `json:"category"`
	PriceMajor float64          `json:"price_major"`
	Currency   catalog.Currency `json:"currency"`
	Reason     string           `json:"reason"`
}

type ProductCard struct {
	ProductID          string             `json:"product_id"`
	Title              string             `json:"title"`
	Brand              string             `json:"brand"`
	Category           string             `json:"category"`
	OriginCountry      string             `json:"origin_country"`
	PriceMajor         float64            `json:"price_major"`
	Currency           string             `json:"currency"`
	SourcePriceMajor   float64            `json:"source_price_major"`
	SourceCurrency     string             `json:"source_currency"`
	Highlights         []string           `json:"highlights"`
	SKUs               []catalog.SKUInfo  `json:"skus"`
	Score              float32            `json:"score"`
	LandedPrice        *LandedPriceInfo   `json:"landed_price,omitempty"`
	SourcePlatform     string             `json:"source_platform"`
	CanonicalProductID string             `json:"canonical_product_id"`
	MaterialTags       []string           `json:"material_tags"`
	WeightKg           float64            `json:"weight_kg"`
	Description        string             `json:"description"`
	RatingSummary      map[string]float64 `json:"rating_summary,omitempty"`
	RatingIsLive       bool               `json:"rating_is_live"`
	ShipsTo            []string           `json:"ships_to"`
	DimensionsCm       map[string]float64 `json:"dimensions_cm"`
	UpdatedAt          string             `json:"updated_at"`
	DefaultSKUID       string             `json:"default_sku_id"`
	ImageURL           string             `json:"image_url"`
	ImageKind          string             `json:"image_kind"`
	ImageAlt           string             `json:"image_alt"`
	SourceLanguage     string             `json:"source_language,omitempty"`
	SourceLocale       string             `json:"source_locale,omitempty"`
	DataProvenance     string             `json:"data_provenance,omitempty"`
}

type SKUInfo struct {
	SKUID      string  `json:"sku_id"`
	Spec       string  `json:"spec"`
	PriceMajor float64 `json:"price_major"`
	Currency   string  `json:"currency"`
	Stock      int     `json:"stock"`
}

type LandedPriceInfo struct {
	SubtotalMajor     float64 `json:"subtotal_major,omitempty"`
	ShippingMajor     float64 `json:"shipping_major,omitempty"`
	TariffMajor       float64 `json:"tariff_major,omitempty"`
	TotalMajor        float64 `json:"total_major,omitempty"`
	Currency          string  `json:"currency,omitempty"`
	UnavailableReason string  `json:"unavailable_reason,omitempty"`
}

var (
	errRerankerNotConfigured     = &useCaseError{code: "reranker_not_configured", message: "精排器未配置"}
	errRerankScoreLengthMismatch = &useCaseError{code: "rerank_score_length_mismatch", message: "精排分数数量与候选数量不匹配"}
)

type useCaseError struct {
	code    string
	message string
}

func (e *useCaseError) Error() string { return e.message }
func (e *useCaseError) Code() string  { return e.code }
func (e *useCaseError) Is(target error) bool {
	ue, ok := target.(*useCaseError)
	return ok && ue.code == e.code
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
