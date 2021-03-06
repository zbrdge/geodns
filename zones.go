package main

import (
	"camlistore.org/pkg/errorutil"
	"encoding/json"
	"fmt"
	"github.com/miekg/dns"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
)

func zonesReader(dirName string, zones Zones) {
	for {
		zonesReadDir(dirName, zones)
		time.Sleep(5 * time.Second)
	}
}

func addHandler(zones Zones, name string, config *Zone) {
	zones[name] = config
	dns.HandleFunc(name, setupServerFunc(config))
}

func zonesReadDir(dirName string, zones Zones) error {
	dir, err := ioutil.ReadDir(dirName)
	if err != nil {
		log.Println("Could not read", dirName, ":", err)
		return err
	}

	seenZones := map[string]bool{}

	var parse_err error

	for _, file := range dir {
		fileName := file.Name()
		if !strings.HasSuffix(strings.ToLower(fileName), ".json") {
			continue
		}

		zoneName := zoneNameFromFile(fileName)

		seenZones[zoneName] = true

		if zone, ok := zones[zoneName]; !ok || file.ModTime().After(zone.LastRead) {
			if ok {
				log.Printf("Reloading %s\n", fileName)
			} else {
				logPrintf("Reading new file %s\n", fileName)
			}

			//log.Println("FILE:", i, file, zoneName)
			config, err := readZoneFile(zoneName, path.Join(dirName, fileName))
			if config == nil || err != nil {
				log.Println("Caught an error", err)
				if config == nil {
					config = new(Zone)
				}
				config.LastRead = file.ModTime()
				zones[zoneName] = config
				parse_err = err
				continue
			}
			config.LastRead = file.ModTime()

			addHandler(zones, zoneName, config)
		}
	}

	for zoneName, zone := range zones {
		if zoneName == "pgeodns" {
			continue
		}
		if ok, _ := seenZones[zoneName]; ok {
			continue
		}
		log.Println("Removing zone", zoneName, zone.Origin)
		dns.HandleRemove(zoneName)
		delete(zones, zoneName)
	}

	return parse_err
}

func setupPgeodnsZone(zones Zones) {
	zoneName := "pgeodns"
	Zone := new(Zone)
	Zone.Labels = make(labels)
	Zone.Origin = zoneName
	Zone.LenLabels = dns.LenLabels(Zone.Origin)
	label := new(Label)
	label.Records = make(map[uint16]Records)
	label.Weight = make(map[uint16]int)
	Zone.Labels[""] = label
	setupSOA(Zone)
	addHandler(zones, zoneName, Zone)
}

func readZoneFile(zoneName, fileName string) (zone *Zone, zerr error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("reading %s failed: %s", zoneName, r)
			debug.PrintStack()
			zerr = fmt.Errorf("reading %s failed: %s", zoneName, r)
		}
	}()

	fh, err := os.Open(fileName)
	if err != nil {
		log.Println("Could not read ", fileName, ": ", err)
		panic(err)
	}

	zone = new(Zone)
	zone.Labels = make(labels)
	zone.Origin = zoneName
	zone.LenLabels = dns.LenLabels(zone.Origin)
	zone.Options.Ttl = 120
	zone.Options.MaxHosts = 2
	zone.Options.Contact = "support.bitnames.com"

	var objmap map[string]interface{}
	decoder := json.NewDecoder(fh)
	if err = decoder.Decode(&objmap); err != nil {
		extra := ""
		if serr, ok := err.(*json.SyntaxError); ok {
			if _, serr := fh.Seek(0, os.SEEK_SET); serr != nil {
				log.Fatalf("seek error: %v", serr)
			}
			line, col, highlight := errorutil.HighlightBytePosition(fh, serr.Offset)
			extra = fmt.Sprintf(":\nError at line %d, column %d (file offset %d):\n%s",
				line, col, serr.Offset, highlight)
		}
		return nil, fmt.Errorf("error parsing JSON object in config file %s%s\n%v",
			fh.Name(), extra, err)
	}

	if err != nil {
		panic(err)
	}
	//log.Println(objmap)

	var data map[string]interface{}

	for k, v := range objmap {
		//log.Printf("k: %s v: %#v, T: %T\n", k, v, v)

		switch k {
		case "ttl", "serial", "max_hosts", "contact":
			switch option := k; option {
			case "ttl":
				zone.Options.Ttl = valueToInt(v)
			case "serial":
				zone.Options.Serial = valueToInt(v)
			case "contact":
				zone.Options.Contact = v.(string)
			case "max_hosts":
				zone.Options.MaxHosts = valueToInt(v)
			}
			continue

		case "data":
			data = v.(map[string]interface{})
		}
	}

	setupZoneData(data, zone)

	//log.Printf("ZO T: %T %s\n", Zones["0.us"], Zones["0.us"])

	//log.Println("IP", string(Zone.Regions["0.us"].IPv4[0].ip))

	return zone, nil
}

func setupZoneData(data map[string]interface{}, Zone *Zone) {

	recordTypes := map[string]uint16{
		"a":     dns.TypeA,
		"aaaa":  dns.TypeAAAA,
		"ns":    dns.TypeNS,
		"cname": dns.TypeCNAME,
		"mx":    dns.TypeMX,
		"alias": dns.TypeMF,
	}

	for dk, dv_inter := range data {

		dv := dv_inter.(map[string]interface{})

		//log.Printf("K %s V %s TYPE-V %T\n", dk, dv, dv)

		label := Zone.AddLabel(dk)

		if ttl, ok := dv["ttl"]; ok {
			label.Ttl = valueToInt(ttl)
		}

		if maxHosts, ok := dv["max_hosts"]; ok {
			label.MaxHosts = valueToInt(maxHosts)
		}

		for rType, dnsType := range recordTypes {

			rdata := dv[rType]

			if rdata == nil {
				//log.Printf("No %s records for label %s\n", rType, dk)
				continue
			}

			//log.Printf("rdata %s TYPE-R %T\n", rdata, rdata)

			records := make(map[string][]interface{})

			switch rdata.(type) {
			case map[string]interface{}:
				// Handle NS map syntax, map[ns2.example.net:<nil> ns1.example.net:<nil>]
				tmp := make([]interface{}, 0)
				for rdata_k, rdata_v := range rdata.(map[string]interface{}) {
					if rdata_v == nil {
						rdata_v = ""
					}
					tmp = append(tmp, []string{rdata_k, rdata_v.(string)})
				}
				records[rType] = tmp
			case string:
				// CNAME and alias
				tmp := make([]interface{}, 1)
				tmp[0] = rdata.(string)
				records[rType] = tmp
			default:
				records[rType] = rdata.([]interface{})
			}

			//log.Printf("RECORDS %s TYPE-REC %T\n", Records, Records)

			label.Records[dnsType] = make(Records, len(records[rType]))

			for i := 0; i < len(records[rType]); i++ {

				//log.Printf("RT %T %#v\n", records[rType][i], records[rType][i])

				record := new(Record)

				var h dns.RR_Header
				// log.Println("TTL OPTIONS", Zone.Options.Ttl)
				h.Ttl = uint32(label.Ttl)
				h.Class = dns.ClassINET
				h.Rrtype = dnsType
				h.Name = label.Label + "." + Zone.Origin + "."

				switch dnsType {
				case dns.TypeA, dns.TypeAAAA:
					rec := records[rType][i].([]interface{})
					ip := rec[0].(string)
					var err error

					if len(rec) > 1 {
						switch rec[1].(type) {
						case string:
							record.Weight, err = strconv.Atoi(rec[1].(string))
							if err != nil {
								panic("Error converting weight to integer")
							}
						case float64:
							record.Weight = int(rec[1].(float64))
						}
					}
					switch dnsType {
					case dns.TypeA:
						if x := net.ParseIP(ip); x != nil {
							record.RR = &dns.A{Hdr: h, A: x}
							break
						}
						panic(fmt.Errorf("Bad A record %s for %s", ip, dk))
					case dns.TypeAAAA:
						if x := net.ParseIP(ip); x != nil {
							record.RR = &dns.AAAA{Hdr: h, AAAA: x}
							break
						}
						panic(fmt.Errorf("Bad AAAA record %s for %s", ip, dk))
					}

				case dns.TypeMX:
					rec := records[rType][i].(map[string]interface{})
					pref := uint16(0)
					mx := rec["mx"].(string)
					if !strings.HasSuffix(mx, ".") {
						mx = mx + "."
					}
					if rec["weight"] != nil {
						record.Weight = valueToInt(rec["weight"])
					}
					if rec["preference"] != nil {
						pref = uint16(valueToInt(rec["preference"]))
					}
					record.RR = &dns.MX{
						Hdr:        h,
						Mx:         mx,
						Preference: pref}

				case dns.TypeCNAME:
					rec := records[rType][i]
					target := rec.(string)
					if !dns.IsFqdn(target) {
						target = target + "." + Zone.Origin
					}
					record.RR = &dns.CNAME{Hdr: h, Target: dns.Fqdn(target)}

				case dns.TypeMF:
					rec := records[rType][i]
					// MF records (how we store aliases) are not FQDNs
					record.RR = &dns.MF{Hdr: h, Mf: rec.(string)}

				case dns.TypeNS:
					rec := records[rType][i]
					if h.Ttl < 86400 {
						h.Ttl = 86400
					}

					var ns string

					switch rec.(type) {
					case string:
						ns = rec.(string)
					case []string:
						recl := rec.([]string)
						ns = recl[0]
						if len(recl[1]) > 0 {
							log.Println("NS records with names syntax not supported")
						}
					default:
						log.Printf("Data: %T %#v\n", rec, rec)
						panic("Unrecognized NS format/syntax")
					}

					rr := &dns.NS{Hdr: h, Ns: dns.Fqdn(ns)}

					record.RR = rr

				default:
					log.Println("type:", rType)
					panic("Don't know how to handle this type")
				}

				if record.RR == nil {
					panic("record.RR is nil")
				}

				label.Weight[dnsType] += record.Weight
				label.Records[dnsType][i] = *record
			}
			if label.Weight[dnsType] > 0 {
				sort.Sort(RecordsByWeight{label.Records[dnsType]})
			}
		}
	}

	setupSOA(Zone)

	//log.Println(Zones[k])
}

func setupSOA(Zone *Zone) {
	label := Zone.Labels[""]

	primaryNs := "ns"

	// log.Println("LABEL", label)

	if label == nil {
		log.Println(Zone.Origin, "doesn't have any 'root' records,",
			"you should probably add some NS records")
		label = Zone.AddLabel("")
	}

	if record, ok := label.Records[dns.TypeNS]; ok {
		primaryNs = record[0].RR.(*dns.NS).Ns
	}

	s := Zone.Origin + ". 3600 IN SOA " +
		primaryNs + " " + Zone.Options.Contact + " " +
		strconv.Itoa(Zone.Options.Serial) +
		" 5400 5400 2419200 " +
		strconv.Itoa(Zone.Options.Ttl)

	// log.Println("SOA: ", s)

	rr, err := dns.NewRR(s)

	if err != nil {
		log.Println("SOA Error", err)
		panic("Could not setup SOA")
	}

	record := Record{RR: rr}

	label.Records[dns.TypeSOA] = make([]Record, 1)
	label.Records[dns.TypeSOA][0] = record

}

func valueToInt(v interface{}) (rv int) {
	switch v.(type) {
	case string:
		i, err := strconv.Atoi(v.(string))
		if err != nil {
			panic("Error converting weight to integer")
		}
		rv = i
	case float64:
		rv = int(v.(float64))
	default:
		log.Println("Can't convert", v, "to integer")
		panic("Can't convert value")
	}
	return rv
}

func zoneNameFromFile(fileName string) string {
	return fileName[0:strings.LastIndex(fileName, ".")]
}
