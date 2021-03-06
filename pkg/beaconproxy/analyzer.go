package beaconproxy

import (
	"math"
	"sort"
	"strconv"
	"sync"

	"github.com/activecm/rita/config"
	"github.com/activecm/rita/database"
	"github.com/activecm/rita/pkg/data"
	"github.com/activecm/rita/pkg/uconnproxy"
	"github.com/activecm/rita/util"

	"github.com/globalsign/mgo/bson"
	log "github.com/sirupsen/logrus"
)

type (
	analyzer struct {
		tsMin            int64                  // min timestamp for the whole dataset
		tsMax            int64                  // max timestamp for the whole dataset
		chunk            int                    //current chunk (0 if not on rolling analysis)
		chunkStr         string                 //current chunk (0 if not on rolling analysis)
		db               *database.DB           // provides access to MongoDB
		conf             *config.Config         // contains details needed to access MongoDB
		log              *log.Logger            // main logger for RITA
		analyzedCallback func(*update)          // called on each analyzed result
		closedCallback   func()                 // called when .close() is called and no more calls to analyzedCallback will be made
		analysisChannel  chan *uconnproxy.Input // holds unanalyzed data
		analysisWg       sync.WaitGroup         // wait for analysis to finish
	}
)

//newAnalyzer creates a new collector for gathering data //
func newAnalyzer(min int64, max int64, chunk int, db *database.DB, conf *config.Config, log *log.Logger,
	analyzedCallback func(*update), closedCallback func()) *analyzer {
	return &analyzer{
		tsMin:            min,
		tsMax:            max,
		chunk:            chunk,
		chunkStr:         strconv.Itoa(chunk),
		db:               db,
		conf:             conf,
		log:              log,
		analyzedCallback: analyzedCallback,
		closedCallback:   closedCallback,
		analysisChannel:  make(chan *uconnproxy.Input),
	}
}

//collect sends a chunk of data to be analyzed
func (a *analyzer) collect(data *uconnproxy.Input) {
	a.analysisChannel <- data
}

//close waits for the collector to finish
func (a *analyzer) close() {
	close(a.analysisChannel)
	a.analysisWg.Wait()
	a.closedCallback()
}

//start kicks off a new analysis thread
func (a *analyzer) start() {
	a.analysisWg.Add(1)
	go func() {

		for entry := range a.analysisChannel {

			// set up beacon writer output
			output := &update{}

			// if uconnproxy has turned into a strobe, we will not have any timestamps here,
			// and we need to update uconnproxy table with the strobe flag. This is being done
			// here and not in uconnproxy because uconnproxy doesn't do reads, and doesn't know
			// the updated conn count
			if (entry.TsList) == nil {

				output.uconnproxy = updateInfo{
					// update hosts record
					query: bson.M{
						"$set": bson.M{"strobeFQDN": true},
					},
					// create selector for output
					selector: entry.Hosts.BSONKey(),
				}

				// set to writer channel
				a.analyzedCallback(output)

			} else {

				// create selector pair object
				selectorPair := entry.Hosts.BSONKey()

				// create query
				query := bson.M{}

				//store the diff slice length since we use it a lot
				//for timestamps this is one less then the data slice length
				//since we are calculating the times in between readings
				tsLength := len(entry.TsList) - 1

				//find the delta times between the timestamps
				diff := make([]int64, tsLength)
				for i := 0; i < tsLength; i++ {
					diff[i] = entry.TsList[i+1] - entry.TsList[i]
				}

				//perfect beacons should have symmetric delta time and size distributions
				//Bowley's measure of skew is used to check symmetry
				sort.Sort(util.SortableInt64(diff))
				tsSkew := float64(0)

				//tsLength -1 is used since diff is a zero based slice
				tsLow := diff[util.Round(.25*float64(tsLength-1))]
				tsMid := diff[util.Round(.5*float64(tsLength-1))]
				tsHigh := diff[util.Round(.75*float64(tsLength-1))]
				tsBowleyNum := tsLow + tsHigh - 2*tsMid
				tsBowleyDen := tsHigh - tsLow

				//tsSkew should equal zero if the denominator equals zero
				//bowley skew is unreliable if Q2 = Q1 or Q2 = Q3
				if tsBowleyDen != 0 && tsMid != tsLow && tsMid != tsHigh {
					tsSkew = float64(tsBowleyNum) / float64(tsBowleyDen)
				}

				//perfect beacons should have very low dispersion around the
				//median of their delta times
				//Median Absolute Deviation About the Median
				//is used to check dispersion
				devs := make([]int64, tsLength)
				for i := 0; i < tsLength; i++ {
					devs[i] = util.Abs(diff[i] - tsMid)
				}

				sort.Sort(util.SortableInt64(devs))

				tsMadm := devs[util.Round(.5*float64(tsLength-1))]

				//Store the range for human analysis
				tsIntervalRange := diff[tsLength-1] - diff[0]

				//get a list of the intervals found in the data,
				//the number of times the interval was found,
				//and the most occurring interval
				intervals, intervalCounts, tsMode, tsModeCount := createCountMap(diff)

				//more skewed distributions receive a lower score
				//less skewed distributions receive a higher score
				tsSkewScore := 1.0 - math.Abs(tsSkew) //smush tsSkew

				//lower dispersion is better, cutoff dispersion scores at 30 seconds
				tsMadmScore := 1.0 - float64(tsMadm)/30.0
				if tsMadmScore < 0 {
					tsMadmScore = 0
				}

				// connection count scoring
				tsConnDiv := (float64(a.tsMax) - float64(a.tsMin)) / 10.0
				tsConnCountScore := float64(entry.ConnectionCount) / tsConnDiv
				if tsConnCountScore > 1.0 {
					tsConnCountScore = 1.0
				}

				//score numerators
				tsSum := tsSkewScore + tsMadmScore + tsConnCountScore

				//score averages
				tsScore := math.Ceil((tsSum/3.0)*1000) / 1000
				score := math.Ceil((tsSum/3.0)*1000) / 1000

				// update beacon query
				query["$set"] = bson.M{
					"connection_count":   entry.ConnectionCount,
					"proxy":              entry.Proxy,
					"src_network_name":   entry.Hosts.SrcNetworkName,
					"ts.range":           tsIntervalRange,
					"ts.mode":            tsMode,
					"ts.mode_count":      tsModeCount,
					"ts.intervals":       intervals,
					"ts.interval_counts": intervalCounts,
					"ts.dispersion":      tsMadm,
					"ts.skew":            tsSkew,
					"ts.conns_score":     tsConnCountScore,
					"ts.score":           tsScore,
					"tslist":             entry.TsList,
					"score":              score,
					"cid":                a.chunk,
					"strobeFQDN":         false,
				}

				// set query
				output.beacon.query = query

				// create selector for output
				output.beacon.selector = selectorPair

				// updates max beacon proxy score for the source entry in the hosts table
				output.hostBeacon = a.hostBeaconQuery(score, entry.Hosts.UniqueSrcIP.Unpair(), entry.Hosts.FQDN)

				// set to writer channel
				a.analyzedCallback(output)
			}
		}

		a.analysisWg.Done()
	}()
}

// createCountMap returns a distinct data array, data count array, the mode,
// and the number of times the mode occurred
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

//countAndRemoveConsecutiveDuplicates removes consecutive
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

func (a *analyzer) hostBeaconQuery(score float64, src data.UniqueIP, fqdn string) updateInfo {
	ssn := a.db.Session.Copy()
	defer ssn.Close()

	var output updateInfo

	// create query
	query := bson.M{}

	// check if we need to update
	// we do this before the other queries because otherwise if a beacon
	// starts out with a high score which reduces over time, it will keep
	// the incorrect high max for that specific destination.
	maxBeaconMatchExactQuery := src.BSONKey()
	maxBeaconMatchExactQuery["dat.mbproxy"] = fqdn

	nExactMatches, err := ssn.DB(a.db.GetSelectedDB()).C(a.conf.T.Structure.HostTable).
		Find(maxBeaconMatchExactQuery).Count()

	if err != nil {
		a.log.WithError(err).WithFields(log.Fields{
			"src":              src.IP,
			"src_network_name": src.NetworkName,
			"fqdn":             fqdn,
		}).Error(
			"Could not check for existing max proxy beacon in hosts collection. " +
				"Refusing to update source's max proxy beacon.",
		)
		return updateInfo{}
	}

	// if we have exact matches, update to new score and return
	if nExactMatches > 0 {
		query["$set"] = bson.M{
			"dat.$.max_beacon_proxy_score": score,
			"dat.$.mbproxy":                fqdn,
			"dat.$.cid":                    a.chunk,
		}

		// create selector for output
		output.query = query

		// using the same find query we created above will allow us to match and
		// update the exact chunk we need to update
		output.selector = maxBeaconMatchExactQuery

		return output
	}

	// The below is only for cases where the ip is not currently listed as a max beacon
	// for a source
	// update max beacon score
	newFlag := false
	updateFlag := false

	// this query will find any matching chunk that is reporting a lower
	// max beacon score than the current one we are working with
	maxBeaconMatchLowerQuery := src.BSONKey()
	maxBeaconMatchLowerQuery["dat"] = bson.M{
		"$elemMatch": bson.M{
			"cid":                    a.chunk,
			"max_beacon_proxy_score": bson.M{"$lte": score},
		},
	}
	// find matching lower chunks
	nLowerMatches, err := ssn.DB(a.db.GetSelectedDB()).C(a.conf.T.Structure.HostTable).
		Find(maxBeaconMatchLowerQuery).Count()

	if err != nil {
		a.log.WithError(err).WithFields(log.Fields{
			"src":              src.IP,
			"src_network_name": src.NetworkName,
			"fqdn":             fqdn,
		}).Error(
			"Could not check for lower scoring max proxy beacon in hosts collection. " +
				"Refusing to update source's max proxy beacon.",
		)
		return updateInfo{}
	}

	// if no matching chunks are found, we will set the new flag
	if nLowerMatches == 0 {

		maxBeaconMatchUpperQuery := src.BSONKey()
		maxBeaconMatchUpperQuery["dat"] = bson.M{
			"$elemMatch": bson.M{
				"cid":                    a.chunk,
				"max_beacon_proxy_score": bson.M{"$gte": score},
			},
		}

		// find matching upper chunks
		nUpperMatches, err := ssn.DB(a.db.GetSelectedDB()).C(a.conf.T.Structure.HostTable).
			Find(maxBeaconMatchUpperQuery).Count()

		if err != nil {
			a.log.WithError(err).WithFields(log.Fields{
				"src":              src.IP,
				"src_network_name": src.NetworkName,
				"fqdn":             fqdn,
			}).Error(
				"Could not check for higher scoring max proxy beacon in hosts collection. " +
					"Refusing to update source's max proxy beacon.",
			)
			return updateInfo{}
		}

		// update if no upper chunks are found
		if nUpperMatches == 0 {
			newFlag = true
		}
	} else {
		updateFlag = true
	}

	// since we didn't find any changeable lower max beacon scores, we will
	// set the condition to push a new entry with the current score listed as the
	// max beacon ONLY if no matching chunks reporting higher max beacon scores
	// are found.

	if newFlag {

		query["$push"] = bson.M{
			"dat": bson.M{
				"max_beacon_proxy_score": score,
				"mbproxy":                fqdn,
				"cid":                    a.chunk,
			}}

		// create selector for output
		output.query = query
		output.selector = src.BSONKey()

	} else if updateFlag {

		query["$set"] = bson.M{
			"dat.$.max_beacon_proxy_score": score,
			"dat.$.mbproxy":                fqdn,
			"dat.$.cid":                    a.chunk,
		}

		// create selector for output
		output.query = query

		// using the same find query we created above will allow us to match and
		// update the exact chunk we need to update
		output.selector = maxBeaconMatchLowerQuery
	}

	return output
}
