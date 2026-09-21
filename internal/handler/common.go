package handler

func UniqueBy[T any, K comparable](
	items []T,
	keyFn func(T) (K, bool),
) []K {
	seen := make(map[K]struct{})
	result := make([]K, 0, len(items))

	for _, item := range items {
		key, ok := keyFn(item)
		if !ok {
			continue
		}

		if _, exists := seen[key]; exists {
			continue
		}

		seen[key] = struct{}{}
		result = append(result, key)
	}

	return result
}
