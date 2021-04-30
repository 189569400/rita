package files

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io/ioutil"
	"os"
	"path"
	"reflect"
	"strconv"
	"strings"
	"time"

	pt "github.com/activecm/rita/parser/parsetypes"
	"github.com/activecm/rita/util"
	log "github.com/sirupsen/logrus"
)

// GatherLogFiles reads the files and directories looking for log and gz files
func GatherLogFiles(paths []string, logger *log.Logger) []string {
	var toReturn []string

	for _, path := range paths {
		if util.IsDir(path) {
			toReturn = append(toReturn, gatherDir(path, logger)...)
		} else if strings.HasSuffix(path, ".gz") ||
			strings.HasSuffix(path, ".log") {
			toReturn = append(toReturn, path)
		} else {
			logger.WithFields(log.Fields{
				"path": path,
			}).Warn("Ignoring non .log or .gz file")
		}
	}

	return toReturn
}

// gatherDir reads the directory looking for log and .gz files
func gatherDir(cpath string, logger *log.Logger) []string {
	var toReturn []string
	files, err := ioutil.ReadDir(cpath)
	if err != nil {
		logger.WithFields(log.Fields{
			"error": err.Error(),
			"path":  cpath,
		}).Error("Error when reading directory")
	}

	for _, file := range files {
		// Stop RITA from following symlinks
		// In the case that RITA is pointed directly at Bro, it should not
		// parse the "current" symlink which points to the spool.
		// if file.IsDir() && file.Mode() != os.ModeSymlink {
		// 	toReturn = append(toReturn, readDir(path.Join(cpath, file.Name()), logger)...)
		// }
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".gz") ||
			strings.HasSuffix(file.Name(), ".log") {
			toReturn = append(toReturn, path.Join(cpath, file.Name()))
		}
	}
	return toReturn
}

// GetFileScanner returns a buffered file scanner for a bro log file
func GetFileScanner(fileHandle *os.File) (*bufio.Scanner, error) {
	ftype := fileHandle.Name()[len(fileHandle.Name())-3:]
	if ftype != ".gz" && ftype != "log" {
		return nil, errors.New("filetype not recognized")
	}

	var scanner *bufio.Scanner
	if ftype == ".gz" {
		rdr, err := gzip.NewReader(fileHandle)
		if err != nil {
			return nil, err
		}
		scanner = bufio.NewScanner(rdr)
	} else {
		scanner = bufio.NewScanner(fileHandle)
	}

	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return scanner, nil
}

// scanHeader scans the comment lines out of a bro file and returns a
// BroHeader object containing the information. NOTE: This has the side
// effect of advancing the fileScanner so that fileScanner.Text() will
// return the first log entry in the file.
func scanTSVHeader(fileScanner *bufio.Scanner) (*BroHeader, error) {
	toReturn := new(BroHeader)
	for fileScanner.Scan() {
		if fileScanner.Err() != nil {
			break
		}
		if len(fileScanner.Text()) < 1 {
			continue
		}
		//On the comment lines
		if fileScanner.Text()[0] == '#' {
			line := strings.Fields(fileScanner.Text())
			switch line[0][1:] {
			case "separator":
				var err error
				toReturn.Separator, err = strconv.Unquote("\"" + line[1] + "\"")
				if err != nil {
					return toReturn, err
				}
			case "set_separator":
				toReturn.SetSep = line[1]
			case "empty_field":
				toReturn.Empty = line[1]
			case "unset_field":
				toReturn.Unset = line[1]
			case "fields":
				toReturn.Names = line[1:]
			case "types":
				toReturn.Types = line[1:]
			case "path":
				toReturn.ObjType = line[1]
			}
		} else {
			//We are done parsing the comments
			break
		}
	}

	if len(toReturn.Names) != len(toReturn.Types) {
		return toReturn, errors.New("name / type mismatch")
	}
	return toReturn, nil
}

//mapBroHeaderToParserType checks a parsed BroHeader against
//a BroData struct and returns a mapping from bro field names in the
//bro header to the indexes of the respective fields in the BroData struct
func mapBroHeaderToParserType(header *BroHeader, broDataFactory func() pt.BroData,
	logger *log.Logger) (BroHeaderIndexMap, error) {
	// The lookup struct gives us a way to walk the data structure only once
	type lookup struct {
		broType string
		offset  int
	}

	//create a bro data to check the header against
	broData := broDataFactory()

	// map the bro names -> the brotypes
	fieldTypes := make(map[string]lookup)

	//toReturn is a simplified version of the fieldTypes map which
	//links a bro field name to its index in the broData struct
	toReturn := make(map[string]int)

	structType := reflect.TypeOf(broData).Elem()

	// walk the fields of the bro data, making sure the bro data struct has
	// an equal number of named bro fields and bro type
	for i := 0; i < structType.NumField(); i++ {
		structField := structType.Field(i)
		broName := structField.Tag.Get("bro")
		broType := structField.Tag.Get("brotype")

		//If this field is not associated with bro, skip it
		if len(broName) == 0 && len(broType) == 0 {
			continue
		}

		if len(broName) == 0 || len(broType) == 0 {
			return nil, errors.New("incomplete bro variable")
		}
		fieldTypes[broName] = lookup{broType: broType, offset: i}
		toReturn[broName] = i
	}

	// walk the header names array and link each field up with a type in the
	// bro data
	for index, name := range header.Names {
		lu, ok := fieldTypes[name]
		if !ok {
			//NOTE: an unmatched field which exists in the log but not the struct
			//is not a fatal error, so we report it and move on
			logger.WithFields(log.Fields{
				"error":         "unmatched field in log",
				"missing_field": name,
			}).Info("the log contains a field with no candidate in the data structure")
			continue
		}

		if header.Types[index] != lu.broType {
			err := errors.New("type mismatch found in log")
			logger.WithFields(log.Fields{
				"error":               err,
				"header.Types[index]": header.Types[index],
				"lu.broType":          lu.broType,
			})
			return nil, err
		}
	}

	return toReturn, nil
}

func ParseJSONLine(lineString string, broDataFactory func() pt.BroData,
	logger *log.Logger) pt.BroData {

	dat := broDataFactory()
	err := json.Unmarshal([]byte(lineString), dat)
	if err != nil {
		logger.WithFields(log.Fields{
			"error": err.Error(),
		}).Error("Encountered unparsable JSON in log")
	}
	dat.ConvertFromJSON()
	return dat
}

func ParseTSVLine(lineString string, header *BroHeader,
	fieldMap BroHeaderIndexMap, broDataFactory func() pt.BroData,
	logger *log.Logger) pt.BroData {

	dat := broDataFactory()
	line := strings.Split(lineString, header.Separator)
	if len(line) < len(header.Names) {
		return nil
	}
	if strings.Contains(line[0], "#") {
		return nil
	}

	data := reflect.ValueOf(dat).Elem()

	for idx, val := range header.Names {
		if line[idx] == header.Empty ||
			line[idx] == header.Unset {
			continue
		}

		//fields not in the struct will not be parsed
		fieldOffset, ok := fieldMap[val]
		if !ok {
			continue
		}

		switch header.Types[idx] {
		case pt.Time:
			secs := strings.Split(line[idx], ".")
			s, err := strconv.ParseInt(secs[0], 10, 64)
			if err != nil {
				logger.WithFields(log.Fields{
					"error": err.Error(),
					"value": line[idx],
				}).Error("Couldn't convert unix ts")
				data.Field(fieldOffset).SetInt(-1)
				break
			}

			n, err := strconv.ParseInt(secs[1], 10, 64)
			if err != nil {
				logger.WithFields(log.Fields{
					"error": err.Error(),
					"value": line[idx],
				}).Error("Couldn't convert unix ts")
				data.Field(fieldOffset).SetInt(-1)
				break
			}

			ttim := time.Unix(s, n)
			tval := ttim.Unix()
			data.Field(fieldOffset).SetInt(tval)
		case pt.String:
			data.Field(fieldOffset).SetString(line[idx])
		case pt.Addr:
			data.Field(fieldOffset).SetString(line[idx])
		case pt.Port:
			pval, err := strconv.ParseInt(line[idx], 10, 32)
			if err != nil {
				logger.WithFields(log.Fields{
					"error": err.Error(),
					"value": line[idx],
				}).Error("Couldn't convert port number")
				data.Field(fieldOffset).SetInt(-1)
				break
			}
			data.Field(fieldOffset).SetInt(pval)
		case pt.Enum:
			data.Field(fieldOffset).SetString(line[idx])
		case pt.Interval:
			flt, err := strconv.ParseFloat(line[idx], 64)
			if err != nil {
				logger.WithFields(log.Fields{
					"error": err.Error(),
					"value": line[idx],
				}).Error("Couldn't convert float")
				data.Field(fieldOffset).SetFloat(-1.0)
				break
			}
			data.Field(fieldOffset).SetFloat(flt)
		case pt.Count:
			cnt, err := strconv.ParseInt(line[idx], 10, 64)
			if err != nil {
				logger.WithFields(log.Fields{
					"error": err.Error(),
					"value": line[idx],
				}).Error("Couldn't convert count")
				data.Field(fieldOffset).SetInt(-1)
				break
			}
			data.Field(fieldOffset).SetInt(cnt)
		case pt.Bool:
			if line[idx] == "T" {
				data.Field(fieldOffset).SetBool(true)
				break
			}
			data.Field(fieldOffset).SetBool(false)
		case pt.StringSet:
			tokens := strings.Split(line[idx], ",")
			tVal := reflect.ValueOf(tokens)
			data.Field(fieldOffset).Set(tVal)
		case pt.EnumSet:
			tokens := strings.Split(line[idx], ",")
			tVal := reflect.ValueOf(tokens)
			data.Field(fieldOffset).Set(tVal)
		case pt.StringVector:
			tokens := strings.Split(line[idx], ",")
			tVal := reflect.ValueOf(tokens)
			data.Field(fieldOffset).Set(tVal)
		case pt.IntervalVector:
			tokens := strings.Split(line[idx], ",")
			floats := make([]float64, len(tokens))
			for i, val := range tokens {
				var err error
				floats[i], err = strconv.ParseFloat(val, 64)
				if err != nil {
					logger.WithFields(log.Fields{
						"error": err.Error(),
						"value": val,
					}).Error("Couldn't convert float")
					break
				}
			}
			fVal := reflect.ValueOf(floats)
			data.Field(fieldOffset).Set(fVal)
		default:
			logger.WithFields(log.Fields{
				"error": "Unhandled type",
				"value": header.Types[idx],
			}).Error("Encountered unhandled type in log")
		}
	}

	return dat
}