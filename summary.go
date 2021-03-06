package quantiles

// SumEntry represents a summary entry
type SumEntry struct {
	value   float64
	weight  float64
	minRank float64
	maxRank float64
}

// Value returns the entries value
func (se SumEntry) Value() float64 {
	return se.value
}

// Weight returns the entries weight
func (se SumEntry) Weight() float64 {
	return se.weight
}

// MaxRank returns the entries maximum rank
func (se SumEntry) MaxRank() float64 {
	return se.maxRank
}

// MinRank returns the entries minimum rank
func (se SumEntry) MinRank() float64 {
	return se.minRank
}

func (se SumEntry) prevMaxRank() float64 {
	return se.maxRank - se.weight
}

func (se SumEntry) nextMinRank() float64 {
	return se.minRank + se.weight
}

// Summary is a summarizes the stream entries
type Summary struct {
	entries []SumEntry
}

// newSummary ...
func newSummary() *Summary {
	return &Summary{
		entries: make([]SumEntry, 0),
	}
}

func (sum *Summary) clone() *Summary {
	newSum := &Summary{
		entries: make([]SumEntry, len(sum.entries)),
	}
	for i, entry := range sum.entries {
		newSum.entries[i] = entry
	}
	return newSum
}

func (sum *Summary) buildFromBufferEntries(bes []bufEntry) {
	sum.entries = make([]SumEntry, len(bes))
	cumWeight := 0.0
	for i, entry := range bes {
		curWeight := entry.weight
		sum.entries[i] = SumEntry{
			value:   entry.value,
			weight:  entry.weight,
			minRank: cumWeight,
			maxRank: cumWeight + curWeight,
		}
		cumWeight += curWeight
	}
}

func (sum *Summary) buildFromSummaryEntries(ses []SumEntry) {
	sum.entries = ses
}

// Merge another summary into the this summary (great for esimating quantiles over several streams)
func (sum *Summary) Merge(other *Summary) {
	otherEntries := other.entries
	if len(otherEntries) == 0 {
		return
	}
	if len(sum.entries) == 0 {
		sum.entries = otherEntries
		return
	}

	baseEntries := sum.entries
	sum.entries = make([]SumEntry, len(baseEntries)+len(otherEntries))

	// Merge entries maintaining ranks. The idea is to stack values
	// in order which we can do in linear time as the two summaries are
	// already sorted. We keep track of the next lower rank from either
	// summary and update it as we pop elements from the summaries.
	// We handle the special case when the next two elements from either
	// summary are equal, in which case we just merge the two elements
	// and simultaneously update both ranks.
	var (
		i            int
		j            int
		nextMinRank1 float64
		nextMinRank2 float64
	)

	num := 0
	for i != len(baseEntries) && j != len(otherEntries) {
		it1 := baseEntries[i]
		it2 := otherEntries[j]
		if it1.value < it2.value {
			sum.entries[num] = SumEntry{
				value: it1.value, weight: it1.weight,
				minRank: it1.minRank + nextMinRank2,
				maxRank: it1.maxRank + it2.prevMaxRank(),
			}
			nextMinRank1 = it1.nextMinRank()
			i++
		} else if it1.value > it2.value {
			sum.entries[num] = SumEntry{
				value: it2.value, weight: it2.weight,
				minRank: it2.minRank + nextMinRank1,
				maxRank: it2.maxRank + it1.prevMaxRank(),
			}
			nextMinRank2 = it2.nextMinRank()
			j++
		} else {
			sum.entries[num] = SumEntry{
				value: it1.value, weight: it1.weight + it2.weight,
				minRank: it1.minRank + it2.minRank,
				maxRank: it1.maxRank + it2.maxRank,
			}
			nextMinRank1 = it1.nextMinRank()
			nextMinRank2 = it2.nextMinRank()
			i++
			j++
		}
		num++
	}

	// Fill in any residual.
	for i != len(baseEntries) {
		it1 := baseEntries[i]
		sum.entries[num] = SumEntry{
			value: it1.value, weight: it1.weight,
			minRank: it1.minRank + nextMinRank2,
			maxRank: it1.maxRank + otherEntries[len(otherEntries)-1].maxRank,
		}
		i++
		num++
	}
	for j != len(otherEntries) {
		it2 := otherEntries[j]
		sum.entries[num] = SumEntry{
			value: it2.value, weight: it2.weight,
			minRank: it2.minRank + nextMinRank1,
			maxRank: it2.maxRank + baseEntries[len(baseEntries)-1].maxRank,
		}
		j++
		num++
	}
	sum.entries = sum.entries[:num]

}

func (sum *Summary) compress(sizeHint int64, minEps float64) {
	// No-op if we're already within the size requirement.
	sizeHint = maxInt64(sizeHint, 2)
	if int64(len(sum.entries)) <= sizeHint {
		return
	}

	// First compute the max error bound delta resulting from this compression.
	epsDelta := sum.TotalWeight() * maxFloat64(1/float64(sizeHint), minEps)

	// Compress elements ensuring approximation bounds and elements diversity are both maintained.
	var (
		addAccumulator int64
		addStep        = int64(len(sum.entries))
	)

	wi := 1
	li := wi

	for ri := 0; ri+1 != len(sum.entries); {
		ni := ri + 1
		for ni != len(sum.entries) && addAccumulator < addStep &&
			sum.entries[ni].prevMaxRank()-sum.entries[ri].nextMinRank() <= epsDelta {
			addAccumulator += sizeHint
			ni++
		}
		if sum.entries[ri] == sum.entries[ni-1] {
			ri++
		} else {
			ri = ni - 1
		}

		sum.entries[wi] = sum.entries[ri]
		wi++
		li = ri
		addAccumulator -= addStep
	}

	if li+1 != len(sum.entries) {
		sum.entries[wi] = sum.entries[len(sum.entries)-1]
		wi++
	}

	sum.entries = sum.entries[:wi]
}

// GenerateBoundaries ...
func (sum *Summary) GenerateBoundaries(numBoundaries int64) []float64 {
	// To construct the boundaries we first run a soft compress over a copy
	// of the summary and retrieve the values.
	// The resulting boundaries are guaranteed to both contain at least
	// num_boundaries unique elements and maintain approximation bounds.
	if len(sum.entries) == 0 {
		return []float64{}
	}

	// Generate soft compressed summary.
	compressedSummary := &Summary{}
	compressedSummary.buildFromSummaryEntries(sum.entries)
	// Set an epsilon for compression that's at most 1.0 / num_boundaries
	// more than epsilon of original our summary since the compression operation
	// adds ~1.0/num_boundaries to final approximation error.
	compressionEps := sum.ApproximationError() + 1.0/float64(numBoundaries)
	compressedSummary.compress(numBoundaries, compressionEps)

	// Return boundaries.
	output := make([]float64, len(compressedSummary.entries))
	for _, entry := range compressedSummary.entries {
		output = append(output, entry.value)
	}
	return output
}

// GenerateQuantiles returns a slice of float64 of size numQuantiles+1, the ith entry is the `i * 1/numQuantiles+1` quantile
func (sum *Summary) GenerateQuantiles(numQuantiles int64) []float64 {
	// To construct the desired n-quantiles we repetitively query n ranks from the
	// original summary. The following algorithm is an efficient cache-friendly
	// O(n) implementation of that idea which avoids the cost of the repetitive
	// full rank queries O(nlogn).
	if len(sum.entries) == 0 {
		return []float64{}
	}
	if numQuantiles < 2 {
		numQuantiles = 2
	}
	curIdx := 0
	output := make([]float64, numQuantiles+1)
	for rank := 0.0; rank <= float64(numQuantiles); rank++ {
		d2 := 2 * (rank * sum.entries[len(sum.entries)-1].maxRank / float64(numQuantiles))
		nextIdx := curIdx + 1
		for nextIdx < len(sum.entries) && d2 >= sum.entries[nextIdx].minRank+sum.entries[nextIdx].maxRank {
			nextIdx++
		}
		curIdx = nextIdx - 1
		// Determine insertion order.
		if nextIdx == len(sum.entries) || d2 < sum.entries[curIdx].nextMinRank()+sum.entries[nextIdx].prevMaxRank() {
			output[int(rank)] = sum.entries[curIdx].value
		} else {
			output[int(rank)] = sum.entries[nextIdx].value
		}
	}
	return output
}

// ApproximationError ...
func (sum *Summary) ApproximationError() float64 {
	if len(sum.entries) == 0 {
		return 0
	}

	var maxGap float64
	for i := 1; i < len(sum.entries); i++ {
		it := sum.entries[i]
		if tmp := it.maxRank - it.minRank - it.weight; tmp > maxGap {
			maxGap = tmp
		}
		if tmp := it.prevMaxRank() - sum.entries[i-1].nextMinRank(); tmp > maxGap {
			maxGap = tmp
		}
	}
	return maxGap / sum.TotalWeight()
}

// MinValue returns the min weight value of the summary
func (sum *Summary) MinValue() float64 {
	if len(sum.entries) != 0 {
		return sum.entries[0].value
	}
	return 0
}

// MaxValue returns the max weight value of the summary
func (sum *Summary) MaxValue() float64 {
	if len(sum.entries) != 0 {
		return sum.entries[len(sum.entries)-1].value
	}
	return 0
}

// TotalWeight returns the total weight of the summary
func (sum *Summary) TotalWeight() float64 {
	if len(sum.entries) != 0 {
		return sum.entries[len(sum.entries)-1].maxRank
	}
	return 0
}

// Size returns the size (num of entries) in the summary
func (sum *Summary) Size() int64 {
	return int64(len(sum.entries))
}

// Clear reset the summary
func (sum *Summary) Clear() {
	sum.entries = []SumEntry{}
}

// Entries returns all summary entries
func (sum *Summary) Entries() []SumEntry {
	return sum.entries
}
