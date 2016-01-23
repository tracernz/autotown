package autotown

import (
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"crypto/sha256"

	"google.golang.org/appengine"
	"google.golang.org/appengine/datastore"
	"google.golang.org/appengine/log"
	"google.golang.org/appengine/memcache"
	"google.golang.org/appengine/taskqueue"
)

func init() {
	http.HandleFunc("/admin/rewriteUUIDs", handleRewriteUUIDs)
	http.HandleFunc("/admin/updateControllers", handleUpdateControllers)
	http.HandleFunc("/admin/exportBoards", handleExportBoards)
	http.HandleFunc("/asyncRollup", handleAsyncRollup)
}

func handleRewriteUUIDs(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)

	q := datastore.NewQuery("TuneResults").Order("-timestamp").Limit(50)
	res := []TuneResults{}
	if err := fillKeyQuery(c, q, &res); err != nil {
		log.Errorf(c, "Error fetching tune results: %v", err)
		http.Error(w, err.Error(), 500)
		return
	}

	var keys []*datastore.Key
	var toUpdate []TuneResults
	for _, x := range res {
		if len(x.UUID) == 64 {
			continue
		}
		prevuuid := x.UUID
		if err := x.uncompress(); err != nil {
			log.Errorf(c, "Error uncompressing %q: %v", x.UUID, err)
			continue
		}
		d := json.NewDecoder(bytes.NewReader(x.Data))
		d.UseNumber()
		m := map[string]interface{}{}
		err := d.Decode(&m)
		if err != nil {
			log.Errorf(c, "Error updating %q: %v", x.UUID, err)
			continue
		}
		x.UUID = fmt.Sprintf("%x", sha256.Sum256([]byte(x.UUID)))
		m["uniqueId"] = x.UUID
		x.Data, err = json.Marshal(m)
		if err != nil {
			log.Errorf(c, "Error encoding %q: %v", x.UUID, err)
			continue
		}
		if err := x.compress(); err != nil {
			log.Errorf(c, "Error compressing %q: %v", x.UUID, err)
			continue
		}
		log.Infof(c, "Updating %q -> %q for %v", prevuuid, x.UUID, x.Key.Encode())
		keys = append(keys, x.Key)
		toUpdate = append(toUpdate, x)
	}

	log.Infof(c, "Updating %v items", len(keys))
	_, err := datastore.PutMulti(c, keys, toUpdate)
	if err != nil {
		log.Errorf(c, "Error udpating tune records: %v", err)
		http.Error(w, err.Error(), 500)
		return
	}

	w.WriteHeader(204)
}

func handleUpdateControllers(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)

	q := datastore.NewQuery("UsageStat")
	var tasks []*taskqueue.Task

	for t := q.Run(c); ; {
		var st UsageStat
		_, err := t.Next(&st)
		if err == datastore.Done {
			break
		}

		err = st.uncompress()
		if err != nil {
			log.Warningf(c, "Failed to decompress record: %v", err)
			continue
		}

		rm := json.RawMessage(st.Data)
		data := &asyncUsageData{
			IP:        st.Addr,
			Country:   st.Country,
			Region:    st.Region,
			City:      st.City,
			Lat:       st.Lat,
			Lon:       st.Lon,
			Timestamp: st.Timestamp,
			RawData:   &rm,
		}

		j, err := json.Marshal(data)
		if err != nil {
			log.Infof(c, "Error marshaling input: %v", err)
			http.Error(w, err.Error(), 500)
			return
		}

		g, err := gz(j)
		if err != nil {
			log.Infof(c, "Error compressing input: %v", err)
			http.Error(w, err.Error(), 500)
			return
		}

		tasks = append(tasks, &taskqueue.Task{
			Path:    "/asyncRollup",
			Payload: g,
		})

		if len(tasks) == 100 {
			_, err := taskqueue.AddMulti(c, tasks, "asyncUsageRollup")
			if err != nil {
				log.Errorf(c, "Error queueing stuff: %v", err)
				http.Error(w, "error queueing", 500)
				return
			}
			tasks = nil
		}

	}

	if tasks != nil {
		_, err := taskqueue.AddMulti(c, tasks, "asyncUsageRollup")
		if err != nil {
			log.Errorf(c, "Error queueing stuff: %v", err)
			http.Error(w, "error queueing", 500)
			return
		}
	}

	w.WriteHeader(204)
}

func handleAsyncRollup(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)

	var d asyncUsageData
	br, err := gzip.NewReader(r.Body)
	if err != nil {
		log.Errorf(c, "Error initializing ungzip: %v", err)
		http.Error(w, "error ungzipping", 500)
		return
	}
	if err := json.NewDecoder(br).Decode(&d); err != nil {
		log.Errorf(c, "Error decoding async json data: %v", err)
		http.Error(w, "error decoding json", 500)
		return
	}

	rec := struct {
		BoardsSeen []struct {
			CPU, UUID string
			FwHash    string
			GitHash   string
			GitTag    string
			Name      string
			UavoHash  string
		}
		CurrentArch, CurrentOS string
		GCSVersion             string `json:"gcs_version"`
		ShareIP                string
	}{}
	if err := json.Unmarshal([]byte(*d.RawData), &rec); err != nil {
		log.Warningf(c, "Couldn't parse %s: %v", *d.RawData, err)
		http.Error(w, "error ungzipping", 500)
		return
	}

	items := map[string]FoundController{}
	for _, b := range rec.BoardsSeen {
		uuid := b.UUID
		if uuid == "" {
			if b.CPU == "" {
				log.Infof(c, "No UUID or CPU ID found for %v", b)
				continue
			}
			uuid = fmt.Sprintf("%x", sha256.Sum256([]byte(b.CPU)))
		}

		if b.Name == "CopterControl" {
			b.Name = "CC3D"
		}

		fc := items[uuid]
		if d.Timestamp.After(fc.Timestamp) {
			fc.UUID = uuid
			fc.Name = b.Name
			fc.GitHash = b.GitHash
			fc.GitTag = b.GitTag
			fc.UAVOHash = b.UavoHash
			fc.GCSOS = rec.CurrentOS
			fc.GCSArch = rec.CurrentArch
			fc.GCSVersion = rec.GCSVersion
			fc.Addr = d.IP
			fc.Country = d.Country
			fc.Region = d.Region
			fc.City = d.City
			fc.Lat = d.Lat
			fc.Lon = d.Lon
			fc.Timestamp = d.Timestamp
			fc.Oldest = d.Timestamp
			if rec.ShareIP != "true" {
				fc.Addr = ""
			}
		}

		if d.Timestamp.Before(fc.Oldest) {
			fc.Oldest = d.Timestamp
		}

		fc.Count++

		items[uuid] = fc
	}

	var keys []*datastore.Key
	var toUpdate []FoundController
	for k, v := range items {
		key := datastore.NewKey(c, "FoundController", k, 0, nil)
		prev := &FoundController{}
		err := datastore.Get(c, key, prev)
		switch err {
		case datastore.ErrNoSuchEntity:
		case nil:
		default:
			log.Errorf(c, "Error fetching tune: %v", err)
			http.Error(w, err.Error(), 500)
			return
		}

		if prev.Oldest.Before(v.Oldest) {
			v.Oldest = prev.Oldest
		}
		if prev.Timestamp.After(v.Timestamp) {
			v.Oldest = prev.Timestamp
		}

		keys = append(keys, key)
		toUpdate = append(toUpdate, v)
	}

	log.Infof(c, "Updating %v items", len(keys))
	_, err = datastore.PutMulti(c, keys, toUpdate)
	if err != nil {
		log.Errorf(c, "Error updating controller records: %v", err)
		http.Error(w, err.Error(), 500)
		return
	}

	memcache.Delete(c, resultsStatsKey)

	w.WriteHeader(204)

}

func abbrevOS(s string) string {
	switch {
	case strings.HasPrefix(s, "Windows"):
		return "Windows"
	case strings.HasPrefix(s, "Ubuntu"), strings.HasPrefix(s, "openSUSE"),
		strings.HasPrefix(s, "Gentoo"), strings.HasPrefix(s, "Arch"):
		return "Linux"
	case strings.HasPrefix(s, "OS X"):
		return "Mac"
	default:
		return s
	}
}

func handleExportBoards(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)

	gitl, err := gitLabels(c)
	if err != nil {
		log.Warningf(c, "Couldn't resolve git labels: %v", err)
	}

	w.Header().Set("Content-Type", "text/plain")

	header := []string{"timestamp", "oldest", "count",
		"uuid", "name", "git_hash", "git_tag", "ref", "uavo_hash",
		"gcs_os", "gcs_os_abbrev", "gcs_arch", "gcs_version",
		"country", "region", "city", "lat", "lon",
	}

	cw := csv.NewWriter(w)
	defer cw.Flush()
	cw.Write(header)

	q := datastore.NewQuery("FoundController").Order("-timestamp")

	for t := q.Run(c); ; {
		var x FoundController
		_, err := t.Next(&x)
		if err == datastore.Done {
			break
		}

		ref := ""
		if lbls := gitDescribe(x.GitHash, gitl); lbls != nil {
			ref = lbls[0].Label
		}

		cw.Write(append([]string{
			x.Timestamp.Format(time.RFC3339), x.Oldest.Format(time.RFC3339),
			fmt.Sprint(x.Count),
			x.UUID, x.Name, x.GitHash, x.GitTag, ref, x.UAVOHash,
			x.GCSOS, abbrevOS(x.GCSOS), x.GCSArch, x.GCSVersion,
			x.Country, x.Region, x.City, fmt.Sprint(x.Lat), fmt.Sprint(x.Lon)},
		))
	}

}
