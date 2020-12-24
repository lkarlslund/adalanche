package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Showmax/go-fqdn"
	"github.com/gofrs/uuid"
	jsoniter "github.com/json-iterator/go"
	"github.com/pierrec/lz4"
	"github.com/rs/zerolog/log"
	"github.com/schollz/progressbar/v3"
	"github.com/tinylib/msgp/msgp"
)

// Install AssetFS compiler:
// go get github.com/go-bindata/go-bindata/...
// go get github.com/elazarl/go-bindata-assetfs/...

//go:generate go-bindata-assetfs html/... readme.MD

var (
	qjson = jsoniter.ConfigCompatibleWithStandardLibrary
)

const (
	programname = "adalanche"
	version     = "2020.10.08"
)

func showUsage() {
	fmt.Printf("Usage: sapience [-options ...] command\n\n")
	fmt.Print(`Commands are:
  dump - to dump an AD into a compressed file
  analyze - launches embedded webservice
  dump-analyze - dumps an AD and launched embedded webservice
  export - save analysis to graph files
`)
	flag.PrintDefaults()
	os.Exit(0)
}

func main() {
	server := flag.String("server", "", "DC to connect to, use IP or full hostname ex. -dc=\"dc.contoso.local\", random DC is auto-detected if not supplied")
	port := flag.Int("port", 636, "LDAP port to connect to (389 or 636 typical)")
	domain := flag.String("domain", "", "domain suffix to analyze (auto-detected if not supplied)")
	user := flag.String("username", "", "username to connect with ex. -username=\"someuser\"")
	pass := flag.String("password", "", "password to connect with ex. -password=\"testpass!\"")
	startTLS := flag.Bool("startTLS", false, "Use for StartTLS on 389. Default is TLS on 636")
	authmodeString := flag.String("authmode", "simple", "Bind mode: unauth, simple, md5, ntml, ntlmpth (password is hash), gssapi")
	authdomain := flag.String("authdomain", "", "domain for authentication, if using ntlm auth")
	unsafe := flag.Bool("unsafe", false, "Use for testing and plaintext connection")
	ignoreCert := flag.Bool("ignorecert", true, "Disable certificate checks")
	datapath := flag.String("datapath", "data", "folder to store cached ldap data")
	dumpquery := flag.String("dumpquery", "(objectClass=*)", "LDAP query for dump, defaults to everything")
	analyzequery := flag.String("analyzequery", "(&(objectClass=group)(|(name=Domain Admins)(name=Enterprise Admins)))", "LDAP query to locate targets for analysis")
	importall := flag.Bool("importall", false, "Load all attributes from dump (expands search options, but at the cost of memory")
	exportinverted := flag.Bool("exportinverted", false, "Invert analysis, discover how much damage targets can do")
	exporttype := flag.String("exporttype", "cytoscapejs", "Graph type to export (cytoscapejs, graphviz)")
	attributesparam := flag.String("attributes", "", "Comma seperated list of attributes to get, blank means everything")
	nosacl := flag.Bool("nosacl", true, "Request data with NO SACL flag, allows normal users to dump ntSecurityDescriptor field")
	pagesize := flag.Int("pagesize", 1000, "Chunk requests into pages of this count of objects")
	bind := flag.String("bind", "127.0.0.1:8080", "Address and port of webservice to bind to")
	nobrowser := flag.Bool("nobrowser", false, "Don't launch browser after starting webservice")

	flag.Parse()

	fmt.Println(programname + " " + version + "\n")

	// Ensure the cache folder is available
	if _, err := os.Stat(*datapath); os.IsNotExist(err) {
		err = os.Mkdir(*datapath, 0600)
		if err != nil {
			log.Fatal().Msgf("Could not create cache folder %v: %v", datapath, err)
		}
	}

	command := "dump-analyze"

	if flag.NArg() < 1 {
		fmt.Println("No command issued, assuming 'dump-analyze'. Try command 'help' to get help.\n")
	} else {
		command = flag.Arg(0)
	}

	// Auto detect domain if not supplied
	if *domain == "" {
		log.Info().Msg("No domain supplied, auto-detecting")
		*domain = strings.ToLower(os.Getenv("USERDNSDOMAIN"))
		if *domain == "" {
			// That didn't work, lets try something else
			f, err := fqdn.FqdnHostname()
			if err == nil {
				*domain = strings.ToLower(f[strings.Index(f, ".")+1:])
			}
		}
		if *domain == "" {
			log.Fatal().Msg("Domain auto-detection failed")
		} else {
			log.Info().Msgf("Auto-detected domain as %v", *domain)
		}
	}

	// Dump data?
	if command == "dump" || command == "dump-analyze" {
		if *domain != "" && *server == "" {
			// Auto-detect server
			cname, servers, err := net.LookupSRV("", "", "_ldap._tcp.dc._msdcs."+*domain)
			if err == nil && cname != "" && len(servers) != 0 {
				*server = servers[0].Target
				log.Info().Msgf("AD controller detected as: %v", *server)
			} else {
				log.Warn().Msg("AD controller auto-detection failed, use -server xxxx parameter")
			}
		}

		if *user == "" {
			// Auto-detect user
			*user = os.Getenv("USERNAME")
			if *user != "" {
				log.Info().Msgf("Auto-detected username as %v", *user)
			}
		}

		if len(*server) != 0 && len(*domain) != 0 && len(*user) != 0 && len(*pass) == 0 {
			log.Fatal().Msg("Please provide password using -password=xxxx paramenter")
		}
		if len(*server) == 0 || len(*domain) == 0 || len(*user) == 0 || len(*pass) == 0 {
			log.Warn().Msg("Provide at least username, password, server, and domain name")
			showUsage()
		}

		var authmode byte
		switch *authmodeString {
		case "unauth":
			authmode = 0
		case "simple":
			authmode = 1
		case "md5":
			authmode = 2
		case "ntlm":
			authmode = 3
		case "ntlmpth":
			authmode = 4
		case "gssapi":
			authmode = 5
		default:
			log.Error().Msgf("Unknown LDAP authentication mode %v", *authmodeString)
			showUsage()
		}

		username := *user + "@" + *domain
		ad := AD{
			Domain:     *domain,
			Server:     *server,
			Port:       uint16(*port),
			User:       username,
			Password:   *pass,
			AuthDomain: *authdomain,
			Unsafe:     *unsafe,
			StartTLS:   *startTLS,
			IgnoreCert: *ignoreCert,
		}

		err := ad.Connect(authmode)
		if err != nil {
			log.Fatal().Msgf("Problem connecting to AD: %v", err)
		}

		var attributes []string
		if *attributesparam != "" {
			attributes = strings.Split(*attributesparam, ",")
		}

		outfile, err := os.Create(filepath.Join(*datapath, *domain+".objects.lz4.msgp"))
		if err != nil {
			log.Fatal().Msgf("Problem opening domain cache file: %v", err)
		}
		boutfile := lz4.NewWriter(outfile)
		boutfile.Header.CompressionLevel = 10
		e := msgp.NewWriter(boutfile)

		dumpbar := progressbar.NewOptions(0,
			progressbar.OptionSetDescription("Dumping..."),
			progressbar.OptionShowCount(),
			progressbar.OptionShowIts(),
			progressbar.OptionSetItsString("objects"),
			progressbar.OptionOnCompletion(func() { fmt.Println() }),
			progressbar.OptionThrottle(time.Second*1),
		)

		log.Info().Msg("Dumping schema objects ...")
		rawobjects, err := ad.Dump("CN=Schema,CN=Configuration,"+ad.RootDn(), *dumpquery, attributes, *nosacl, *pagesize)
		if err != nil {
			log.Fatal().Msgf("Problem dumping AD: %v", err)
		}
		log.Printf("Saving %v schema objects ...", len(rawobjects))
		for _, object := range rawobjects {
			err = object.EncodeMsg(e)
			if err != nil {
				log.Fatal().Msgf("Problem encoding LDAP object %v: %v", object.DistinguishedName, err)
			}
			dumpbar.Add(1)
		}

		log.Info().Msg("Dumping configuration objects ...")
		rawobjects, err = ad.Dump("CN=Configuration,"+ad.RootDn(), *dumpquery, attributes, *nosacl, *pagesize)
		if err != nil {
			log.Fatal().Msgf("Problem dumping AD: %v", err)
		}
		log.Printf("Saving %v configuration objects ...", len(rawobjects))
		for _, object := range rawobjects {
			err = object.EncodeMsg(e)
			if err != nil {
				log.Fatal().Msgf("Problem encoding LDAP object %v: %v", object.DistinguishedName, err)
			}
			dumpbar.Add(1)
		}

		log.Info().Msg("Dumping forest DNS objects ...")
		rawobjects, err = ad.Dump("DC=ForestDnsZones,"+ad.RootDn(), *dumpquery, attributes, *nosacl, *pagesize)
		if err != nil {
			log.Warn().Msgf("Problem dumping forest DNS zones (maybe it doesn't exist): %v", err)
		} else {
			log.Printf("Saving %v forest DNS objects ...", len(rawobjects))
			for _, object := range rawobjects {
				err = object.EncodeMsg(e)
				if err != nil {
					log.Fatal().Msgf("Problem encoding LDAP object %v: %v", object.DistinguishedName, err)
				}
				dumpbar.Add(1)
			}
		}
		log.Info().Msg("Dumping domain DNS objects ...")
		rawobjects, err = ad.Dump("DC=DomainDnsZones,"+ad.RootDn(), *dumpquery, attributes, *nosacl, *pagesize)
		if err != nil {
			log.Warn().Msgf("Problem dumping domain DNS zones (maybe it doesn't exist): %v", err)
		} else {
			log.Printf("Saving %v domain DNS objects ...", len(rawobjects))
			for _, object := range rawobjects {
				err = object.EncodeMsg(e)
				if err != nil {
					log.Fatal().Msgf("Problem encoding LDAP object %v: %v", object.DistinguishedName, err)
				}
				dumpbar.Add(1)
			}
		}

		log.Info().Msg("Dumping main AD objects ...")
		rawobjects, err = ad.Dump(ad.RootDn(), *dumpquery, attributes, *nosacl, *pagesize)
		if err != nil {
			log.Fatal().Msgf("Problem dumping AD: %v", err)
		}
		log.Printf("Saving %v AD objects ...", len(rawobjects))
		for _, object := range rawobjects {
			err = object.EncodeMsg(e)
			if err != nil {
				log.Fatal().Msgf("Problem encoding LDAP object %v: %v", object.DistinguishedName, err)
			}
			dumpbar.Add(1)
		}
		dumpbar.Finish()

		err = ad.Disconnect()
		if err != nil {
			log.Fatal().Msgf("Problem disconnecting from AD: %v", err)
		}

		e.Flush()
		boutfile.Close()
		outfile.Close()

	}

	if command == "dump" {
		os.Exit(0)
	}

	// Everything else requires us to load data
	if len(*domain) == 0 {
		log.Error().Msg("Please provide domain name")
		showUsage()
	}

	for _, domain := range strings.Split(*domain, ",") {
		cachefile, err := os.Open(filepath.Join(*datapath, domain+".objects.lz4.msgp"))
		if err != nil {
			log.Fatal().Msgf("Problem opening domain cache file: %v", err)
		}
		bcachefile := lz4.NewReader(cachefile)

		cachestat, _ := cachefile.Stat()

		loadbar := progressbar.NewOptions(int(cachestat.Size()),
			progressbar.OptionSetDescription("Loading objects from "+domain+" ..."),
			progressbar.OptionShowBytes(true),
			progressbar.OptionThrottle(time.Second*1),
			progressbar.OptionOnCompletion(func() { fmt.Println() }),
		)

		d := msgp.NewReader(bcachefile)
		// d := msgp.NewReader(&progressbar.Reader{bcachefile, &loadbar})

		// Load all the stuff
		var lastpos int64
		for {
			var rawObject RawObject
			err = rawObject.DecodeMsg(d)

			pos, _ := cachefile.Seek(0, io.SeekCurrent)
			loadbar.Add(int(pos - lastpos))
			lastpos = pos

			if err == nil {
				newObject := rawObject.ToObject(*importall)
				AllObjects.Add(&newObject)
			} else if msgp.Cause(err) == io.EOF {
				break
			} else {
				log.Fatal().Msgf("Problem decoding object: %v", err)
			}
		}
		cachefile.Close()
		loadbar.Finish()
	}

	log.Printf("Loaded %v ojects", len(AllObjects.AsArray()))

	// Add our known SIDs if they're missing
	for sid, name := range knownsids {
		binsid, err := SIDFromString(sid)
		if err != nil {
			log.Fatal().Msgf("Problem parsing SID %v", sid)
		}
		if _, found := AllObjects.FindSID(binsid); !found {
			AllObjects.Add(&Object{
				DistinguishedName: "CN=" + name + ",DN=microsoft-builtin",
				Attributes: map[Attribute][]string{
					Name:           {name},
					ObjectSid:      {string(binsid)},
					ObjectClass:    {"person", "user", "top"},
					ObjectCategory: {"Group"},
				},
			})
		}
	}

	// ShowAttributePopularity()

	// Generate member of chains
	processbar := progressbar.NewOptions(int(len(AllObjects.dnmap)),
		progressbar.OptionSetDescription("Processing objects..."),
		progressbar.OptionShowCount(),
		progressbar.OptionShowIts(),
		progressbar.OptionSetItsString("objects"),
		progressbar.OptionOnCompletion(func() { fmt.Println() }),
		progressbar.OptionThrottle(time.Second*1),
	)

	everyonesid, _ := SIDFromString("S-1-1-0")
	authenticateduserssid, _ := SIDFromString("S-1-5-11")
	everyone := AllObjects.FindOrAddSID(everyonesid)
	authenticatedusers := AllObjects.FindOrAddSID(authenticateduserssid)

	log.Info().Msg("Pre-processing directory data ...")
	for _, object := range AllObjects.AsArray() {
		processbar.Add(1)
		object.MemberOf()

		// Crude special handling for Everyone and Authenticated Users
		if object.Type() == ObjectTypeUser || object.Type() == ObjectTypeComputer || object.Type() == ObjectTypeManagedServiceAccount {
			everyone.imamemberofyou(object)
			authenticatedusers.imamemberofyou(object)
			object.memberof = append(object.memberof, everyone, authenticatedusers)
		}

		object.SetAttr(MetaType, object.Type().String())
		if lastlogon, ok := object.AttrTimestamp(LastLogonTimestamp); ok {
			object.SetAttr(MetaLastLoginAge, strconv.Itoa(int(time.Since(lastlogon)/time.Hour)))
		}
		if passwordlastset, ok := object.AttrTimestamp(PwdLastSet); ok {
			object.SetAttr(MetaPasswordAge, strconv.Itoa(int(time.Since(passwordlastset)/time.Hour)))
		}
		if strings.Contains(strings.ToLower(object.OneAttr(OperatingSystem)), "linux") {
			object.SetAttr(MetaLinux, "1")
		}
		if strings.Contains(strings.ToLower(object.OneAttr(OperatingSystem)), "windows") {
			object.SetAttr(MetaWindows, "1")
		}
		if uac, ok := object.AttrInt(UserAccountControl); ok {
			if uac&UAC_TRUSTED_FOR_DELEGATION != 0 {
				object.SetAttr(MetaUnconstrainedDelegation, "1")
			}
			if uac&UAC_TRUSTED_TO_AUTH_FOR_DELEGATION != 0 {
				object.SetAttr(MetaConstrainedDelegation, "1")
			}
			if uac&UAC_NOT_DELEGATED != 0 {
				log.Printf("%v has can't be used as delegation", object.DN())
			}
			if uac&UAC_WORKSTATION_TRUST_ACCOUNT != 0 {
				object.SetAttr(MetaWorkstation, "1")
			}
			if uac&UAC_SERVER_TRUST_ACCOUNT != 0 {
				object.SetAttr(MetaServer, "1")
			}
			if uac&UAC_ACCOUNTDISABLE != 0 {
				object.SetAttr(MetaAccountDisabled, "1")
			}
			if uac&UAC_PASSWD_CANT_CHANGE != 0 {
				object.SetAttr(MetaPasswordCantChange, "1")
			}
			if uac&UAC_DONT_EXPIRE_PASSWORD != 0 {
				object.SetAttr(MetaPasswordNoExpire, "1")
			}
			if uac&UAC_PASSWD_NOTREQD != 0 {
				object.SetAttr(MetaPasswordNotRequired, "1")
			}
		}

		if object.Type() == ObjectTypeTrust {
			// http://www.frickelsoft.net/blog/?p=211
			var direction string
			dir, _ := object.AttrInt(TrustDirection)
			switch dir {
			case 0:
				direction = "disabled"
			case 1:
				direction = "incoming"
			case 2:
				direction = "outgoing"
			case 3:
				direction = "bidirectional"
			}

			attr, _ := object.AttrInt(TrustAttributes)
			log.Printf("Domain has a %v trust with %v", direction, object.OneAttr(TrustPartner))
			if dir&2 != 0 && attr&4 != 0 {
				log.Printf("SID filtering is not enabled, so pwn %v and pwn this AD too", object.OneAttr(TrustPartner))
			}
		}

		// Special types of Objects
		if object.HasAttrValue(ObjectClass, "controlAccessRight") {
			u, err := uuid.FromString(object.OneAttr(A("rightsGuid")))
			// log.Printf("Adding right %v %v", u, object.OneAttr(DisplayName))
			if err == nil {
				AllRights[u] = object
			}
		} else if object.HasAttrValue(ObjectClass, "attributeSchema") {
			objectGUID, err := uuid.FromBytes([]byte(object.OneAttr(A("schemaIDGUID"))))
			objectGUID = SwapUUIDEndianess(objectGUID)
			// log.Printf("Adding schema attribute %v %v", u, object.OneAttr(Name))
			if err == nil {
				AllSchemaAttributes[objectGUID] = object
				switch object.OneAttr(Name) {
				case "ms-Mcs-AdmPwd":
					log.Info().Msg("Detected LAPS schema extension, adding extra analyzer")
					PwnAnalyzers = append(PwnAnalyzers, PwnAnalyzer{
						Method: PwnReadLAPSPassword,
						ObjectAnalyzer: func(o *Object) []*Object {
							var results []*Object
							// Only for computers
							if o.Type() != ObjectTypeComputer {
								return results
							}
							// ... that has LAPS installed
							if len(o.Attr(MSmcsAdmPwdExpirationTime)) == 0 {
								return results
							}
							// Analyze ACL
							sd, err := o.SecurityDescriptor()
							if err != nil {
								return results
							}
							for _, acl := range sd.DACL.Entries {
								if acl.Type == ACETYPE_ACCESS_ALLOWED_OBJECT && acl.Mask&RIGHT_DS_READ_PROPERTY != 0 && acl.ObjectType == objectGUID {
									results = append(results, AllObjects.FindOrAddSID(acl.SID))
								}
							}
							return results
						},
					})
				}
			}
		} else if object.HasAttrValue(ObjectClass, "classSchema") {
			u, err := uuid.FromBytes([]byte(object.OneAttr(A("schemaIDGUID"))))
			u = SwapUUIDEndianess(u)
			// log.Printf("Adding schema class %v %v", u, object.OneAttr(Name))
			if err == nil {
				AllSchemaClasses[u] = object
			}
		}
	}
	processbar.Finish()

	// Generate member of chains
	pwnbar := progressbar.NewOptions(int(len(AllObjects.dnmap)),
		progressbar.OptionSetDescription("Analyzing who can pwn who ..."),
		progressbar.OptionShowCount(),
		progressbar.OptionShowIts(),
		progressbar.OptionSetItsString("objects"),
		// progressbar.OptionShowBytes(true),
		progressbar.OptionOnCompletion(func() { fmt.Println() }),
		progressbar.OptionThrottle(time.Second*1),
	)

	var pwnlinks int
	for _, object := range AllObjects.AsArray() {
		pwnbar.Add(1)
		// log.Info().Msg(object.String())
		for _, analyzer := range PwnAnalyzers {
			for _, pwnobject := range analyzer.ObjectAnalyzer(object) {
				if pwnobject == object || pwnobject.SID() == object.SID() { // SID check solves (some) dual-AD analysis problems
					// We don't care about self owns
					continue
				}

				// Ignore these, SELF = self own, Creator/Owner always has full rights
				if pwnobject.SID() == SelfSID || pwnobject.SID() == CreatorOwnerSID || pwnobject.SID() == SystemSID {
					continue
				}
				// log.Printf("Detected that %v can pwn %v by %v", pwnobject.DN(), object.DN(), analyzer.Method)
				pwnobject.CanPwn = append(pwnobject.CanPwn, PwnInfo{Method: analyzer.Method, Target: object})
				object.PwnableBy = append(object.PwnableBy, PwnInfo{Method: analyzer.Method, Target: pwnobject})
				pwnlinks++
			}
		}
	}
	pwnbar.Finish()
	log.Printf("Detected %v ways to pwn objects", pwnlinks)

	switch command {
	case "exportacls":
		log.Info().Msg("Finding most valuable assets ...")

		output, err := os.Create("debug.txt")
		if err != nil {
			log.Fatal().Msgf("Error opening output file: %v", err)
		}

		for _, object := range AllObjects.AsArray() {
			fmt.Fprintf(output, "Object:\n%v\n\n-----------------------------\n", object)
		}
		output.Close()

		log.Info().Msg("Done")
	case "export":
		log.Info().Msg("Finding most valuable assets ...")
		q, err := ParseQueryStrict(*analyzequery)
		if err != nil {
			log.Fatal().Msgf("Error parsing LDAP query: %v", err)
		}

		includeobjects := AllObjects.Filter(func(o *Object) bool {
			return q.Evaluate(o)
		})

		mode := "normal"
		if *exportinverted {
			mode = "inverted"
		}
		resultgraph := AnalyzeObjects(includeobjects, nil, PwnMethodValues() /* all */, mode, 99)

		switch *exporttype {
		case "graphviz":
			err = ExportGraphViz(resultgraph, "sapience-"+*domain+".dot")
		case "cytoscapejs":
			err = ExportCytoscapeJS(resultgraph, "cytoscape-js-"+*domain+".json")
		default:
			fmt.Println("Unknown export format")
			showUsage()
		}
		if err != nil {
			log.Fatal().Msgf("Problem exporting graph: %v", err)
		}

		log.Info().Msg("Done")
	case "analyze", "dump-analyze":
		quit := make(chan bool)

		srv := webservice(*bind)

		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatal().Msgf("Problem launching webservice listener: %s", err)
			} else {
				quit <- true
			}
		}()

		// Launch browser
		if !*nobrowser {
			var err error
			url := "http://" + *bind
			switch runtime.GOOS {
			case "linux":
				err = exec.Command("xdg-open", url).Start()
			case "windows":
				err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
			case "darwin":
				err = exec.Command("open", url).Start()
			default:
				err = fmt.Errorf("unsupported platform")
			}
			if err != nil {
				log.Printf("Problem launching browser: %v", err)
			}
		}

		// Wait for webservice to end
		<-quit
	default:
		fmt.Printf("Unknown command %v\n\n", flag.Arg(0))
		showUsage()
	}
}