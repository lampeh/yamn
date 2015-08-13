package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
)

// Posting example
func upload() {
	target_url := "https://bingo.mixmin.net:9090/upload"
	filename := "hw.txt"
	err := postFile(filename, target_url)
	if err != nil {
		fmt.Println(err)
	}
	return
}

// postFile uploads a file to a listening webserver
func postFile(filename string, targetUrl string) error {
	bodyBuf := &bytes.Buffer{}
	bodyWriter := multipart.NewWriter(bodyBuf)

	// this step is very important
	fileWriter, err := bodyWriter.CreateFormFile("uploadfile", filename)
	if err != nil {
		fmt.Println("error writing to buffer")
		return err
	}

	// Open the local file to transfer
	fh, err := os.Open(filename)
	if err != nil {
		fmt.Println("Cannot open file")
		return err
	}

	// Copy data from opened file to the Form
	_, err = io.Copy(fileWriter, fh)
	if err != nil {
		fmt.Println("Cannot copy handler")
		return err
	}

	contentType := bodyWriter.FormDataContentType()
	bodyWriter.Close()

	// Create a customized http Transport to override http.Post defaults
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr}

	// Post the completed Form
	resp, err := client.Post(targetUrl, contentType, bodyBuf)
	if err != nil {
		fmt.Println("Connection refused")
		return err
	}
	defer resp.Body.Close()
	// Uncomment this if we want to handle a return from the server
	/*
		resp_body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
	*/

	// Print the HTTP status code
	fmt.Printf("status: %s\n", resp.Status)
	return nil
}

// This function is a closure that returns a function that handles
// inbound web requests.
func makeUploadHandler(maxLen int64) http.Handler {
	// Define the form template
	form := `<html>
<head>
<title>Upload file</title>
</head>
<body>
<form enctype="multipart/form-data" action="/upload" method="post">
    <input type="file" name="uploadfile" />
    <input type="submit" value="upload" />
</form>
</body>
</html>`

	t, err := template.New("yamn").Parse(form)
	if err != nil {
		panic(err)
	}
	// Here begins the enclosed function
	fn := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			t.Execute(w, nil)
		} else {
			if r.ContentLength > maxLen {
				http.Error(
					w,
					"Content too large",
					http.StatusExpectationFailed,
				)
				return
			}
			//TODO The size limit should be a yamn const
			r.ParseMultipartForm(16 << 12)
			file, handler, err := r.FormFile("uploadfile")
			if err != nil {
				fmt.Println(err)
				return
			}
			defer file.Close()
			//fmt.Fprintf(w, "%v", handler.Header)
			f, err := os.OpenFile(
				"./pool/"+handler.Filename,
				os.O_WRONLY|os.O_CREATE,
				0666,
			)
			if err != nil {
				fmt.Println(err)
				return
			}
			defer f.Close()
			io.Copy(f, file)
		}
	}
	return http.HandlerFunc(fn)
}

func webserver() {
	mux := http.NewServeMux()
	maxUploadBytes := int64(65535)

	uploadHandler := makeUploadHandler(maxUploadBytes)

	mux.Handle("/upload", uploadHandler)
	//err := http.ListenAndServe(":9090", mux) // setting listening port
	err := http.ListenAndServeTLS(
		":9090",
		"cert.pem",
		"key.pem",
		mux,
	)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
	return
}
