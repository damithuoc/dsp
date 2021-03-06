package dsp_flights

import (
	"encoding/json"
	"fmt"
	"github.com/clixxa/dsp/bindings"
	"github.com/clixxa/dsp/rtb_types"
	"log"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Uses environment variables and real database connections to create Runtimes
type BidEntrypoint struct {
	demandFlight atomic.Value

	BindingDeps bindings.BindingDeps
	Logic       BiddingLogic
	AllTest     bool
}

func (e *BidEntrypoint) Cycle() error {
	// create template demand flight
	df := &DemandFlight{}
	if old, found := e.demandFlight.Load().(*DemandFlight); found {
		e.BindingDeps.Debug.Println("using old runtime")
		df.Runtime = old.Runtime
	} else {
		df.Runtime.Logger = e.BindingDeps.Logger
		df.Runtime.Logger.Println("brand new runtime")
		df.Runtime.Debug = e.BindingDeps.Debug
		df.Runtime.Storage.Recalls = bindings.Recalls{Env: e.BindingDeps}.Save
		s := strings.Split(e.BindingDeps.DefaultKey, ":")
		key, iv := s[0], s[1]
		df.Runtime.DefaultB64 = &bindings.B64{Key: []byte(key), IV: []byte(iv)}
		df.Runtime.Logic = e.Logic
		df.Runtime.TestOnly = e.AllTest

		if err := (bindings.StatsDB{}).Marshal(e.BindingDeps.StatsDB); err != nil {
			e.BindingDeps.Debug.Println("err:", err.Error())
			return err
		}
	}

	if err := df.Runtime.Storage.Folders.Unmarshal(1, e.BindingDeps); err != nil {
		e.BindingDeps.Debug.Println("err:", err.Error())
		return err
	}
	if err := df.Runtime.Storage.Creatives.Unmarshal(1, e.BindingDeps); err != nil {
		e.BindingDeps.Debug.Println("err:", err.Error())
		return err
	}

	if err := df.Runtime.Storage.Users.Unmarshal(1, e.BindingDeps); err != nil {
		e.BindingDeps.Debug.Println("err:", err.Error())
		return err
	}
	if err := df.Runtime.Storage.Pseudonyms.Unmarshal(1, e.BindingDeps); err != nil {
		e.BindingDeps.Debug.Println("err:", err.Error())
		return err
	}

	e.demandFlight.Store(df)
	return nil
}

func (e *BidEntrypoint) DemandFlight() *DemandFlight {
	sf := e.demandFlight.Load().(*DemandFlight)
	flight := &DemandFlight{}
	flight.Runtime = sf.Runtime
	return flight
}

func (e *BidEntrypoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	request := e.DemandFlight()
	request.HttpRequest = r
	request.HttpResponse = w
	request.Launch()
}

type BiddingLogic interface {
	SelectFolderAndCreative(flight *DemandFlight, folders []ElegibleFolder, totalCpc int)
	CalculateRevshare(flight *DemandFlight) float64
	GenerateClickID(*DemandFlight) string
}

type SimpleLogic struct {
}

func (s SimpleLogic) SelectFolderAndCreative(flight *DemandFlight, folders []ElegibleFolder, totalCpc int) {
	eg := folders[flight.Request.RawRequest.Random255%len(folders)]
	foldIds := make([]string, len(folders))
	for n, folder := range folders {
		foldIds[n] = strconv.Itoa(folder.FolderID)
	}
	flight.Runtime.Logger.Println(`folders`, strings.Join(foldIds, ","), `to choose from, picked`, eg.FolderID)
	flight.FolderID = eg.FolderID
	flight.FullPrice = eg.BidAmount
	folder := flight.Runtime.Storage.Folders.ByID(eg.FolderID)
	flight.CreativeID = folder.Creative[flight.Request.RawRequest.Random255%len(folder.Creative)]
}

func (s SimpleLogic) CalculateRevshare(flight *DemandFlight) float64 { return 98.0 }

func (s SimpleLogic) GenerateClickID(*DemandFlight) string { return "" }

type DemandFlight struct {
	Runtime struct {
		DefaultB64 *bindings.B64
		Storage    struct {
			Folders    bindings.Folders
			Creatives  bindings.Creatives
			Pseudonyms bindings.Pseudonyms
			Users      bindings.Users

			Recalls func(json.Marshaler, *error, *int)
		}
		Logger   *log.Logger
		Debug    *log.Logger
		TestOnly bool
		Logic    BiddingLogic
	} `json:"-"`

	HttpRequest  *http.Request       `json:"-"`
	HttpResponse http.ResponseWriter `json:"-"`

	FolderID   int     `json:"folder"`
	CreativeID int     `json:"creative"`
	Request    Request `json:"req"`
	Margin     int     `json:"margin"`
	StartTime  time.Time

	RecallID  int    `json:"-"`
	FullPrice int    `json:"-"`
	WinUrl    string `json:"-"`

	Response rtb_types.Response `json:"-"`
	Error    error              `json:"-"`
}

type dfProxy DemandFlight

func (df *DemandFlight) MarshalJSON() ([]byte, error) {
	return json.Marshal((*dfProxy)(df))
}

func (df *DemandFlight) String() string {
	e := "nil"
	if df.Error != nil {
		e = df.Error.Error()
	}
	return fmt.Sprintf(`demandflight e%s`, e)
}

func (df *DemandFlight) Launch() {
	defer func() {
		if err := recover(); err != nil {
			df.Runtime.Logger.Println("uncaught panic, stack trace following", err)
			s := debug.Stack()
			df.Runtime.Logger.Println(string(s))
		}
	}()
	ReadBidRequest(df)
	FindClient(df)
	PrepareResponse(df)
	WriteBidResponse(df)
}

func ReadBidRequest(flight *DemandFlight) {
	flight.Runtime.Logger.Println(`starting ReadBidRequest!`)
	flight.StartTime = time.Now()

	if e := json.NewDecoder(flight.HttpRequest.Body).Decode(&flight.Request.RawRequest); e != nil {
		flight.Error = e
		flight.Runtime.Logger.Println(`failed to decode body`, e.Error())
	}

	flight.WinUrl = `http://` + flight.HttpRequest.Host + `/win?price=${AUCTION_PRICE}&key=${AUCTION_BID_ID}&imp=${AUCTION_IMP_ID}`

	if dim, found := flight.Runtime.Storage.Pseudonyms.Subnetworks[flight.Request.RawRequest.Site.SubNetwork]; !found {
		flight.Runtime.Logger.Printf(`dim not found %s`, flight.Request.RawRequest.Site.SubNetwork)
	} else {
		flight.Request.SubNetworkID = dim
	}

	if dim, found := flight.Runtime.Storage.Pseudonyms.Countries[flight.Request.RawRequest.Device.Geo.Country]; !found {
		flight.Runtime.Logger.Printf(`dim not found %s`, flight.Request.RawRequest.Device.Geo.Country)
	} else {
		flight.Request.CountryID = dim
	}

	if dim, found := flight.Runtime.Storage.Pseudonyms.Networks[flight.Request.RawRequest.Site.Network]; !found {
		flight.Runtime.Logger.Printf(`dim not found %s`, flight.Request.RawRequest.Site.Network)
	} else {
		flight.Request.NetworkID = dim
	}

	if dim, found := flight.Runtime.Storage.Pseudonyms.DeviceTypes[flight.Request.RawRequest.Device.DeviceType]; !found {
		flight.Runtime.Logger.Printf(`dim not found %s`, flight.Request.RawRequest.Device.DeviceType)
	} else {
		flight.Request.DeviceTypeID = dim
	}

	if dim, found := flight.Runtime.Storage.Pseudonyms.BrandSlugs[flight.Request.RawRequest.Site.Brand]; !found {
		flight.Runtime.Logger.Printf(`dim not found %s`, flight.Request.RawRequest.Site.Brand)
	} else {
		flight.Request.BrandID = dim
	}

	if dim, found := flight.Runtime.Storage.Pseudonyms.Verticals[flight.Request.RawRequest.Site.Vertical]; !found {
		flight.Runtime.Logger.Printf(`dim not found %s`, flight.Request.RawRequest.Site.Vertical)
	} else {
		flight.Request.VerticalID = dim
	}

	if dim, found := flight.Runtime.Storage.Pseudonyms.NetworkTypes[flight.Request.RawRequest.Site.NetworkType]; !found {
		flight.Runtime.Logger.Printf(`dim not found %s`, flight.Request.RawRequest.Site.NetworkType)
	} else {
		flight.Request.NetworkTypeID = dim
	}

	if dim, found := flight.Runtime.Storage.Pseudonyms.Genders[flight.Request.RawRequest.User.Gender]; !found {
		flight.Runtime.Logger.Printf(`dim not found %s`, flight.Request.RawRequest.User.Gender)
	} else {
		flight.Request.GenderID = dim
	}

	flight.Runtime.Logger.Println("dimensions decoded:", flight.Request)
}

// Fill out the elegible bid
func FindClient(flight *DemandFlight) {
	flight.Runtime.Logger.Println(`starting FindClient`, flight.String())
	if flight.Error != nil {
		return
	}

	FolderMatches := func(folder *bindings.Folder) string {
		if !folder.Active {
			return "Inactive"
		}
		if flight.Request.RawRequest.Test {
			goto CheckBrand
		}

		if len(folder.Country) > 0 {
			for _, c := range folder.Country {
				if flight.Request.CountryID == c {
					goto CheckBrand
				}
			}
			return "Country"
		}
	CheckBrand:
		if len(folder.Brand) > 0 {
			for _, v := range folder.Brand {
				if flight.Request.BrandID == v {
					goto CheckNetwork
				}
			}
			return "Brand"
		}
	CheckNetwork:
		if len(folder.Network) > 0 {
			for _, v := range folder.Network {
				if flight.Request.NetworkID == v {
					goto CheckNetworkType
				}
			}
			return "Network"
		}
	CheckNetworkType:
		if len(folder.NetworkType) > 0 {
			for _, v := range folder.NetworkType {
				if flight.Request.NetworkTypeID == v {
					goto CheckSubNetwork
				}
			}
			return "NetworkType"
		}
	CheckSubNetwork:
		if len(folder.SubNetwork) > 0 {
			for _, v := range folder.SubNetwork {
				if flight.Request.SubNetworkID == v {
					goto CheckGender
				}
			}
			return "SubNetwork"
		}
	CheckGender:
		if len(folder.Gender) > 0 {
			for _, v := range folder.Gender {
				if flight.Request.GenderID == v {
					goto CheckDeviceType
				}
			}
			return "Gender"
		}
	CheckDeviceType:
		if len(folder.DeviceType) > 0 {
			for _, v := range folder.DeviceType {
				if flight.Request.DeviceTypeID == v {
					goto CheckVertical
				}
			}
			return "DeviceType"
		}
	CheckVertical:
		if len(folder.Vertical) > 0 {
			for _, v := range folder.Vertical {
				if flight.Request.VerticalID == v {
					goto CheckBidfloor
				}
			}
			return "Vertical"
		}
	CheckBidfloor:
		if folder.CPC > 0 && folder.CPC < flight.Request.RawRequest.Impressions[0].BidFloor {
			return "CPC"
		}
		return ""
	}

	folders := []ElegibleFolder{}
	totalCpc := 0

	Visit := func(folder *bindings.Folder) bool {
		if s := FolderMatches(folder); s != "" {
			flight.Runtime.Logger.Printf("folder %d doesn't match cause %s..", folder.ID, s)
			return false
		}

		flight.Runtime.Logger.Printf("folder %d matches..", folder.ID)

		if len(folder.Creative) > 0 {
			cpc := folder.CPC
			if folder.ParentID != nil && cpc == 0 {
				cpc = flight.Runtime.Storage.Folders.ByID(*folder.ParentID).CPC
			}
			totalCpc += cpc
			folders = append(folders, ElegibleFolder{FolderID: folder.ID, BidAmount: cpc})
		}

		return true
	}

	for _, folder := range flight.Runtime.Storage.Folders {
		if folder.ParentID == nil {
			if !Visit(folder) {
				continue
			}
			for _, r := range folder.Children {
				if !Visit(flight.Runtime.Storage.Folders.ByID(r)) {
					continue
				}
			}
		}
	}

	if len(folders) == 0 {
		flight.Runtime.Logger.Println(`no folder found`)
		return
	}

	flight.Runtime.Logic.SelectFolderAndCreative(flight, folders, totalCpc)
}

func PrepareResponse(flight *DemandFlight) {
	if flight.FolderID == 0 {
		return
	}
	revShare := flight.Runtime.Logic.CalculateRevshare(flight)
	if revShare > 100 {
		revShare = 100
	}
	bid := rtb_types.Bid{}
	fp := float64(flight.FullPrice)
	flight.Runtime.Logger.Printf("rev calculated at %f", revShare)
	bid.Price = fp * revShare / 100
	flight.Margin = flight.FullPrice - int(bid.Price)

	net, found := flight.Runtime.Storage.Pseudonyms.NetworkIDS[flight.Request.NetworkID]
	if !found {
		flight.Runtime.Logger.Printf(`net not found %d`, flight.Request.NetworkID)
		net = ""
	}
	snet, found := flight.Runtime.Storage.Pseudonyms.SubnetworkIDS[flight.Request.SubNetworkID]
	if !found {
		flight.Runtime.Logger.Printf(`snet not found %d`, flight.Request.SubNetworkID)
		snet = ""
	}
	brand, found := flight.Runtime.Storage.Pseudonyms.BrandIDS[flight.Request.BrandID]
	if !found {
		flight.Runtime.Logger.Printf(`brand not found %d`, flight.Request.BrandID)
		brand = ""
	}
	brandSlug, found := flight.Runtime.Storage.Pseudonyms.BrandSlugIDS[flight.Request.BrandID]
	if !found {
		flight.Runtime.Logger.Printf(`brandSlug not found %d`, flight.Request.BrandID)
		brandSlug = ""
	}
	vert, found := flight.Runtime.Storage.Pseudonyms.VerticalIDS[flight.Request.VerticalID]
	if !found {
		flight.Runtime.Logger.Printf(`vert not found %d`, flight.Request.VerticalID)
		vert = ""
	}

	ct := flight.Runtime.Logic.GenerateClickID(flight)

	flight.Runtime.Logger.Println(`saving reference to KVS`)

	flight.Runtime.Storage.Recalls(flight, &flight.Error, &flight.RecallID)
	bid.ID = strconv.Itoa(flight.RecallID)

	bid.WinUrl = flight.WinUrl

	clickid := flight.Runtime.DefaultB64.Encrypt([]byte(fmt.Sprintf(`%d`, flight.RecallID)))

	cr := flight.Runtime.Storage.Creatives.ByID(flight.CreativeID)
	url := cr.RedirectUrl
	url = strings.Replace(url, `{realnetwork}`, "", 1)
	url = strings.Replace(url, `{realsubnetwork}`, "", 1)
	url = strings.Replace(url, `{ct}`, ct, 1)
	url = strings.Replace(url, `{clickid}`, fmt.Sprintf(`%s`, clickid), 1)

	url = strings.Replace(url, `{network}`, fmt.Sprintf(`%s`, net), 1)
	url = strings.Replace(url, `{subnetwork}`, fmt.Sprintf(`%s`, snet), 1)
	url = strings.Replace(url, `{brand}`, fmt.Sprintf(`%s`, brand), 1)
	url = strings.Replace(url, `{brandurl}`, fmt.Sprintf(`%s`, brandSlug), 1)
	url = strings.Replace(url, `{vertical}`, fmt.Sprintf(`%s`, vert), 1)

	url = strings.Replace(url, `{cpc}`, fmt.Sprintf(`%f`, fp/100000), 1)
	url = strings.Replace(url, `{placement}`, flight.Request.RawRequest.Site.Placement, 1)

	bid.URL = url

	if flight.Error != nil {
		flight.Runtime.Logger.Println(`error occured in FindClient: %s`, flight.Error.Error())
		return
	}

	flight.Response.SeatBids = append(flight.Response.SeatBids, rtb_types.SeatBid{Bids: []rtb_types.Bid{bid}})
	flight.Runtime.Logger.Println("finished FindClient", flight.String())
}

func WriteBidResponse(flight *DemandFlight) {
	var res []byte
	if flight.Runtime.TestOnly && len(flight.Response.SeatBids) > 0 && !flight.Request.RawRequest.Test {
		flight.Runtime.Logger.Println(`test traffic only and traffic is non-test, removing bid`, flight.Response.SeatBids)
		flight.Response.SeatBids = nil
	}

	if len(flight.Response.SeatBids) > 0 {
		if j, e := json.Marshal(flight.Response); e != nil && flight.Error == nil {
			flight.Error = e
			flight.Runtime.Logger.Println(`error encoding`, e.Error())
		} else {
			res = j
		}
	}

	if flight.Error != nil {
		flight.Runtime.Logger.Printf("err during request %s, returning 500", flight.Error.Error())
		flight.HttpResponse.WriteHeader(http.StatusInternalServerError)
	} else if res != nil {
		flight.Runtime.Logger.Printf(`looks good and has a response, returning code %d`, http.StatusOK)
		flight.HttpResponse.Header().Set(`Content-Length`, strconv.Itoa(len(res)))
		flight.HttpResponse.WriteHeader(http.StatusOK)
		if n, e := flight.HttpResponse.Write(res); e != nil {
			flight.Runtime.Logger.Printf(`got an error writing so returning 500! wrote %d bytes: %s`, n, e.Error())
		}
	} else {
		flight.Runtime.Logger.Printf(`looks good but no response, returning code %d`, http.StatusNoContent)
		flight.HttpResponse.WriteHeader(http.StatusNoContent)
	}
	flight.Runtime.Logger.Println(`dsp /bid took`, time.Since(flight.StartTime))
}

type Request struct {
	RawRequest rtb_types.Request

	VerticalID    int
	BrandID       int
	NetworkID     int
	SubNetworkID  int
	NetworkTypeID int
	DeviceTypeID  int
	CountryID     int
	GenderID      int
}

type ElegibleFolder struct {
	FolderID  int
	BidAmount int
}
