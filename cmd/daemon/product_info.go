package main

import (
	"encoding/xml"
	"strings"
)

type ProductInfo struct {
	XMLName  xml.Name `xml:"products"`
	Products []struct {
		XMLName      xml.Name `xml:"product"`
		Type         string   `xml:"type"`
		HeadFilesUrl string   `xml:"head-files-url"`
		ImageUrl     string   `xml:"image-url"`
		Autoplay     string   `xml:"autoplay"`
	} `xml:"product"`
}

func (pi ProductInfo) ImageUrl(fileId string) string {
	if len(pi.Products) == 0 {
		return ""
	}
	lastProduct := pi.Products[len(pi.Products)-1]
	return strings.Replace(lastProduct.ImageUrl, "{file_id}", strings.ToLower(fileId), 1)
}


func (pi ProductInfo) AutoplayEnabled() bool {
	return pi.Products[0].Autoplay == "1"
}
