package main

import (
	"bufio"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	lumber "github.com/jcelliott/lumber"
	_ "github.com/lib/pq"
	"log"
	"os"
	"strings"
	"time"
)

const (
	VERSION   = 1.0
	MAX_CONNS = 25
)

var logger *lumber.FileLogger

func getGenotype(db *sql.DB, genotype_id string) (id, user_id, filetype string) {
	stmt, err := db.Prepare("SELECT id, user_id, filetype FROM genotypes WHERE id = $1")
	if err != nil {
		die(err.Error())
	}
	err = stmt.QueryRow(genotype_id).Scan(&id, &user_id, &filetype)

	switch {
	case err == sql.ErrNoRows:
		die("ERROR: Couldn't get genotyping from database, no rows in result set")
	case err != nil:
		die(err.Error())
	default:
		logger.Debug("Got genotyping with id " + id + " and userID " + user_id)
	}

	return id, user_id, filetype
}

func getUserSNPs(db *sql.DB, user_id string) (known_user_snps map[string]bool) {
	//  load the known user-snps
	known_user_snps = make(map[string]bool)
	rows, err := db.Query("SELECT user_snps.snp_name FROM user_snps WHERE user_snps.user_id = $1", user_id)
	if err != nil {
		die(err.Error())
	} else if rows != nil {
		for rows.Next() {
			var snp_name string
			if err := rows.Scan(&snp_name); err != nil {
				die(err.Error())
			}
			known_user_snps[snp_name] = true
		}
		if err := rows.Err(); err != nil {
			die(err.Error())
		}
	}
	return known_user_snps
}

func getSNPs(db *sql.DB) (known_snps map[string]bool) {
	known_snps = make(map[string]bool) // There is no set-type, so this is a workaround
	logger.Info("Loading all SNPs...")
	rows, err := db.Query("SELECT name FROM snps;")
	if err != nil {
		die(err.Error())
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			die(err.Error())
		}
		known_snps[name] = true
	}

	if err := rows.Err(); err != nil {
		die(err.Error())
	}
	return
}

func main() {
	// Get the database, possible values: development, production, test

	var (
		database          string
		username          string
		password          string
		port              string
		genotype_id       string
		temp_file         string
		root_path         string
		connection_string string
	)

	flag.StringVar(&database, "database", "", "Name of the Rails database this worker runs in.")
	flag.StringVar(&password, "password", "", "Password for db")
	flag.StringVar(&username, "username", "", "Username for db")
	flag.StringVar(&port, "port", "", "Port for db")
	flag.StringVar(&genotype_id, "genotype_id", "-1", "ID of the genotype we're parsing")
	flag.StringVar(&temp_file, "temp_file", "", "Path of the file we're parsing")
	flag.StringVar(&root_path, "root_path", "", "Root path of Rails database")
	version := flag.Bool("v", false, "prints current version")

	flag.Parse()

	if *version {
		fmt.Println("Version is:", VERSION)
		os.Exit(0)
	}

	// A map to switch names for known SNPs
	db_snp_snps := map[string]string{
		"MT-T3027C": "rs199838004", "MT-T4336C": "rs41456348",
		"MT-G4580A": "rs28357975", "MT-T5004C": "rs41419549",
		"MT-C5178a": "rs28357984", "MT-A5390G": "rs41333444",
		"MT-C6371T": "rs41366755", "MT-G8697A": "rs28358886",
		"MT-G9477A": "rs2853825", "MT-G10310A": "rs41467651",
		"MT-A10550G": "rs28358280", "MT-C10873T": "rs2857284",
		"MT-C11332T": "rs55714831", "MT-A11947G": "rs28359168",
		"MT-A12308G": "rs2853498", "MT-A12612G": "rs28359172",
		"MT-T14318C": "rs28357675", "MT-T14766C": "rs3135031",
		"MT-T14783C": "rs28357680",
	}
	if root_path == "" {
		fmt.Println("ERROR: Root-path is empty")
		os.Exit(1)
	}
	logger, _ = lumber.NewFileLogger(root_path+"/log/go_parser.log", lumber.INFO, lumber.ROTATE, 5000, 9, 0)

	logger.Info("Started worker")
	logger.Info("Checking for genotype with id " + genotype_id)

	// Now open the single_temp_file and create userSNPs
	logger.Info("Started work on " + temp_file)
	//var file *os.File
	file, err := os.Open(temp_file)
	if err != nil {
		die(err.Error())
	}
	defer file.Close()

	// Connect to database
	connection_string = buildDbConnectionString(username, password, database, port)
	logger.Debug("Trying to connect to the db with params: " + connection_string)
	db, err := sql.Open("postgres", connection_string)
	if err != nil {
		die(err.Error())
	}
	db.SetMaxIdleConns(MAX_CONNS)
	defer db.Close()

	logger.Info("Connected.")

	// Now load the known SNPs
	known_snps := getSNPs(db)
	logger.Info("Got all SNPs.")

	logger.Info("Getting genotype")
	geno_id, user_id, filetype := getGenotype(db, genotype_id)
	fmt.Println(geno_id, user_id, filetype)
	logger.Info("Got filetype '" + filetype + "' and user-id '" + user_id + "'.")
	logger.Info("Getting all user-SNPs.")

	known_user_snps := getUserSNPs(db, user_id)
	logger.Info("Now doing actual parsing.")

	// Turn off AUTOCOMMIT by using BEGIN / INSERTs / COMMIT
	// More tips at http://www.postgresql.org/docs/current/interactive/populate.html,
	// TODO: Implement more improvements, maybe use PREPARE or even just COPY?
	db.Exec("BEGIN")

	// Reset the scanner to the very first line, for example, IYG has already data in the first line
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			// Skip comments
			continue
		}
		line = strings.ToLower(strings.Trim(line, "\n"))
		// Fix the linelist for all different filetypes
		var linelist []string
		if filetype == "23andme" {
			// Nothing much to do for 23andme
			linelist = strings.Split(line, "\t")
		} else if filetype == "ancestry" {
			linelist = strings.Split(line, "\t")
			if linelist[0] == "rsid" {
				continue
			}
			linelist = []string{linelist[0], linelist[1], linelist[3], linelist[4] + linelist[5]}
		} else if filetype == "decodeme" {
			linelist = strings.Split(line, ",")
			log.Println(linelist)
			if linelist[0] == "name" {
				// skip header
				continue
			}
			linelist = []string{linelist[0], linelist[2], linelist[3], linelist[5]}
			log.Println(linelist)
		} else if filetype == "ftdna-illumina" {
			// Remove "
			line = strings.Replace(line, `"`, "", -1) // Backticks are needed here.
			linelist = strings.Split(line, ",")
			if linelist[0] == "rsid" {
				// skip header
				continue
			}
			// Interestingly, from here on ftdna has the same format as 23andme
		} else if filetype == "23andme-exome-vcf" {
			// This is a valid VCF so a bit more work is needed
			linelist = strings.Split(line, "\t")
			format_array := strings.Split(linelist[8], ":")
			genotype_index := -1
			for index, element := range format_array {
				if element == "GT" || element == "gt" {
					genotype_index = index
					break
				}
			}
			non_genotype_parsed := strings.Split(strings.Split(linelist[9], ":")[genotype_index], "/")
			genotype_parsed := ""
			for _, allele := range non_genotype_parsed {
				if allele == "0" {
					genotype_parsed = genotype_parsed + linelist[3]
				} else if allele == "1" {
					genotype_parsed = genotype_parsed + linelist[4]
				}
			}
			linelist = []string{strings.ToLower(linelist[2]), linelist[0], linelist[1], genotype_parsed}

		} else if filetype == "IYG" {
			linelist = strings.Split(line, "\t")
			name := linelist[0]
			// Have to get the position from the name
			// TODO: This is an ugly hack - first, replace all runes
			// which are letters by X, then replace that X by nothing
			replace_letters := func(r rune) rune {
				switch {
				case r >= 'A' && r <= 'Z':
					return 'X'
				case r >= 'a' && r <= 'z':
					return 'X'
				}
				return r
			}
			position := strings.Map(replace_letters, name)
			position = strings.Replace(position, "X", "", -1)
			if strings.HasPrefix(name, "MT") || strings.HasPrefix(name, "mt") {
				// Check whether we have to replace the name with the correct rs ID
				new_name, ok := db_snp_snps[name]
				if ok {
					name = new_name
				}
				linelist = []string{name, "MT", position, linelist[1]}
			} else {
				linelist = []string{linelist[0], "1", "1", linelist[1]}
			}

		} else {
			logger.Info("unknown filetype", filetype)
			err := errors.New("Unknown filetype in parsing")
			die(err.Error())
		}

		// Example:
		// ["rs123", "11", "421412", "aa"]
		snp_name := linelist[0]
		chromosome := strings.ToUpper(linelist[1]) // mt -> MT
		position := linelist[2]
		allele := strings.ToUpper(linelist[3])
		// Is this a known SNP?
		_, ok := known_snps[snp_name]
		if !ok {
			// Create a new SNP
			time := time.Now().UTC().Format(time.RFC3339)
			// possibly TODO: Initialize the genotype frequencies, allele frequencies
			allele_frequency := "---\nA: 0\nT: 0\nG: 0\nC: 0\n"
			genotype_frequency := "--- {}\n"
			insertion_string := "INSERT INTO snps (name, chromosome, position, ranking, allele_frequency, genotype_frequency, user_snps_count, created_at, updated_at) VALUES ('" + snp_name + "','" + chromosome + "','" + position + "','0','" + allele_frequency + "', '" + genotype_frequency + "', '1','" + time + "', '" + time + "');"
			_, err := db.Exec(insertion_string)
			if err != nil {
				die(err.Error())
			}
		}
		// Is this a known userSNP?
		_, ok = known_user_snps[snp_name]
		if !ok {
			// Create a new userSNP
			time := time.Now().Format(time.RFC3339)
			// snp_id is deprecated, just use snp_name
			user_snp_insertion_string := "INSERT INTO user_snps (local_genotype, genotype_id, user_id, created_at, updated_at, snp_name) VALUES ('" + allele + "','" + genotype_id + "','" + user_id + "','" + time + "','" + time + "','" + snp_name + "');"
			_, err := db.Exec(user_snp_insertion_string)
			if err != nil {
				die(err.Error())
			}
		} else {
			logger.Info("User-SNP " + snp_name + " with allele " + allele + " already exists")
		}

	} // End of file-parsing
	logger.Info("Running COMMIT")
	_, err = db.Exec("COMMIT")
	if err != nil {
		logger.Info("Error during COMMIT:")
		die(err.Error())
	}
	// Update our indexes
	// Both of these should only take a few seconds
	logger.Info("VACUUMing...")
	db.Exec("VACUUM ANALYZE snps")
	db.Exec("VACUUM ANALYZE user_snps")
	logger.Info("Done!")
	os.Exit(0)
}

func buildDbConnectionString(username string, password string, database string, port string) string {
	params := make([]string, 0, 5)
	if len(username) > 0 {
		params = append(params, "user="+username)
	}
	if len(password) > 0 {
		params = append(params, "password="+password)
	}
	if len(port) > 0 {
		params = append(params, "port="+port)
	}
	params = append(params, "dbname="+database)
	params = append(params, "sslmode=disable")
	return strings.Join(params, " ")
}

func die(message string) {
	logger.Fatal(message)
	log.Println(message)
	os.Exit(1)
}