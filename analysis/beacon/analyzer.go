package beacon

import (
	"math"
	"sort"
	"sync"

	"github.com/activecm/rita/datatypes/beacon"

	"github.com/activecm/rita/util"
)

type (
	// analyzer implements the bulk of beaconing analysis, creating the scores
	// for a given set of timestamps and data sizes
	analyzer struct {
		connectionThreshold int                          // the minimum number of connections to be considered a beacon
		minTime             int64                        // beginning of the observation period
		maxTime             int64                        // ending of the observation period
		analyzedCallback    func(*beacon.AnalysisOutput) // called on each analyzed result
		closedCallback      func()                       // called when .close() is called and no more calls to analyzedCallback will be made
		analysisChannel     chan *beacon.AnalysisInput   // holds unanalyzed data
		analysisWg          sync.WaitGroup               // wait for analysis to finish
	}
)

// newAnalyzer creates a new analyzer for computing beaconing scores.
func newAnalyzer(minTime, maxTime int64,
	analyzedCallback func(*beacon.AnalysisOutput), closedCallback func()) *analyzer {
	return &analyzer{
		minTime:          minTime,
		maxTime:          maxTime,
		analyzedCallback: analyzedCallback,
		closedCallback:   closedCallback,
		analysisChannel:  make(chan *beacon.AnalysisInput),
	}
}

// analyze sends a group of timestamps and data sizes in for analysis.
// Note: this function may block
func (a *analyzer) analyze(data *beacon.AnalysisInput) {
	a.analysisChannel <- data
}

// close waits for the analysis threads to finish
func (a *analyzer) close() {
	close(a.analysisChannel)
	a.analysisWg.Wait()
	a.closedCallback()
}

// start kicks off a new analysis thread
func (a *analyzer) start() {
	a.analysisWg.Add(1)
	go func() {
		for data := range a.analysisChannel {

			if data.TsList == nil || data.OrigIPBytes == nil {
				continue
			}

			//sort the size and timestamps since they may have arrived out of order
			sort.Sort(util.SortableInt64(data.TsList))
			sort.Sort(util.SortableInt64(data.OrigIPBytes))

			//store the diff slice length since we use it a lot
			//for timestamps this is one less then the data slice length
			//since we are calculating the times in between readings
			tsLength := len(data.TsList) - 1
			dsLength := len(data.OrigIPBytes)

			//find the duration of this connection
			//perfect beacons should fill the observation period
			duration := float64(data.TsList[tsLength]-data.TsList[0]) /
				float64(a.maxTime-a.minTime)

			//find the delta times between the timestamps
			diff := make([]int64, tsLength)
			for i := 0; i < tsLength; i++ {
				diff[i] = data.TsList[i+1] - data.TsList[i]
			}

			//perfect beacons should have symmetric delta time and size distributions
			//Bowley's measure of skew is used to check symmetry
			sort.Sort(util.SortableInt64(diff))
			tsSkew := float64(0)
			dsSkew := float64(0)

			//tsLength -1 is used since diff is a zero based slice
			tsLow := diff[util.Round(.25*float64(tsLength-1))]
			tsMid := diff[util.Round(.5*float64(tsLength-1))]
			tsHigh := diff[util.Round(.75*float64(tsLength-1))]
			tsBowleyNum := tsLow + tsHigh - 2*tsMid
			tsBowleyDen := tsHigh - tsLow

			//we do the same for datasizes
			dsLow := data.OrigIPBytes[util.Round(.25*float64(dsLength-1))]
			dsMid := data.OrigIPBytes[util.Round(.5*float64(dsLength-1))]
			dsHigh := data.OrigIPBytes[util.Round(.75*float64(dsLength-1))]
			dsBowleyNum := dsLow + dsHigh - 2*dsMid
			dsBowleyDen := dsHigh - dsLow

			//tsSkew should equal zero if the denominator equals zero
			//bowley skew is unreliable if Q2 = Q1 or Q2 = Q3
			if tsBowleyDen != 0 && tsMid != tsLow && tsMid != tsHigh {
				tsSkew = float64(tsBowleyNum) / float64(tsBowleyDen)
			}

			if dsBowleyDen != 0 && dsMid != dsLow && dsMid != dsHigh {
				dsSkew = float64(dsBowleyNum) / float64(dsBowleyDen)
			}

			//perfect beacons should have very low dispersion around the
			//median of their delta times
			//Median Absolute Deviation About the Median
			//is used to check dispersion
			devs := make([]int64, tsLength)
			for i := 0; i < tsLength; i++ {
				devs[i] = util.Abs(diff[i] - tsMid)
			}

			dsDevs := make([]int64, dsLength)
			for i := 0; i < dsLength; i++ {
				dsDevs[i] = util.Abs(data.OrigIPBytes[i] - dsMid)
			}

			sort.Sort(util.SortableInt64(devs))
			sort.Sort(util.SortableInt64(dsDevs))

			tsMadm := devs[util.Round(.5*float64(tsLength-1))]
			dsMadm := dsDevs[util.Round(.5*float64(dsLength-1))]

			//Store the range for human analysis
			tsIntervalRange := diff[tsLength-1] - diff[0]
			dsRange := data.OrigIPBytes[dsLength-1] - data.OrigIPBytes[0]

			//get a list of the intervals found in the data,
			//the number of times the interval was found,
			//and the most occurring interval
			intervals, intervalCounts, tsMode, tsModeCount := createCountMap(diff)
			dsSizes, dsCounts, dsMode, dsModeCount := createCountMap(data.OrigIPBytes)

			//more skewed distributions receive a lower score
			//less skewed distributions receive a higher score
			tsSkewScore := 1.0 - math.Abs(tsSkew) //smush tsSkew
			dsSkewScore := 1.0 - math.Abs(dsSkew) //smush dsSkew

			//lower dispersion is better, cutoff dispersion scores at 30 seconds
			tsMadmScore := 1.0 - float64(tsMadm)/30.0
			if tsMadmScore < 0 {
				tsMadmScore = 0
			}

			//lower dispersion is better, cutoff dispersion scores at 32 bytes
			dsMadmScore := 1.0 - float64(dsMadm)/32.0
			if dsMadmScore < 0 {
				dsMadmScore = 0
			}

			tsDurationScore := duration

			//smaller data sizes receive a higher score
			dsSmallnessScore := 1.0 - float64(dsMode)/65535.0
			if dsSmallnessScore < 0 {
				dsSmallnessScore = 0
			}

			output := &beacon.AnalysisOutput{
				UconnID:          data.ID,
				Src:              data.Src,
				Dst:              data.Dst,
				ConnectionCount:  data.ConnectionCount,
				AverageBytes:     data.AverageBytes,
				TSISkew:          tsSkew,
				TSIDispersion:    tsMadm,
				TSDuration:       duration,
				TSIRange:         tsIntervalRange,
				TSIMode:          tsMode,
				TSIModeCount:     tsModeCount,
				TSIntervals:      intervals,
				TSIntervalCounts: intervalCounts,
				DSSkew:           dsSkew,
				DSDispersion:     dsMadm,
				DSRange:          dsRange,
				DSSizes:          dsSizes,
				DSSizeCounts:     dsCounts,
				DSMode:           dsMode,
				DSModeCount:      dsModeCount,
			}

			//score numerators
			tsSum := tsSkewScore + tsMadmScore + tsDurationScore
			dsSum := dsSkewScore + dsMadmScore + dsSmallnessScore

			//score averages
			output.TSScore = tsSum / 3.0
			output.DSScore = dsSum / 3.0
			output.Score = (tsSum + dsSum) / 6.0
			a.analyzedCallback(output)
		}
		a.analysisWg.Done()
	}()
}

// createCountMap returns a distinct data array, data count array, the mode,
// and the number of times the mode occured
func createCountMap(sortedIn []int64) ([]int64, []int64, int64, int64) {
	//Since the data is already sorted, we can call this without fear
	distinct, countsMap := countAndRemoveConsecutiveDuplicates(sortedIn)
	countsArr := make([]int64, len(distinct))
	mode := distinct[0]
	max := countsMap[mode]
	for i, datum := range distinct {
		count := countsMap[datum]
		countsArr[i] = count
		if count > max {
			max = count
			mode = datum
		}
	}
	return distinct, countsArr, mode, max
}

//CountAndRemoveConsecutiveDuplicates removes consecutive
//duplicates in an array of integers and counts how many
//instances of each number exist in the array.
//Similar to `uniq -c`, but counts all duplicates, not just
//consecutive duplicates.
func countAndRemoveConsecutiveDuplicates(numberList []int64) ([]int64, map[int64]int64) {
	//Avoid some reallocations
	result := make([]int64, 0, len(numberList)/2)
	counts := make(map[int64]int64)

	last := numberList[0]
	result = append(result, last)
	counts[last]++

	for idx := 1; idx < len(numberList); idx++ {
		if last != numberList[idx] {
			result = append(result, numberList[idx])
		}
		last = numberList[idx]
		counts[last]++
	}
	return result, counts
}
