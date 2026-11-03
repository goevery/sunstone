package workload

import (
	"fmt"
	"testing"
)

func TestValidateHostnameAcceptsDNSNames(t *testing.T) {
	for _, hostname := range []string{"storefront", "storefront.example.com", "a-b.example.com", "0.example", strings253()} {
		if err := ValidateHostname(hostname); err != nil {
			t.Fatalf("rejected valid hostname %q: %v", hostname, err)
		}
	}
}

func TestValidateHostnameRejectsOtherValues(t *testing.T) {
	for _, hostname := range []string{
		"", "Storefront.example.com", "storefront.example.com.", "storefront.example.com:80",
		"https://storefront.example.com", "storefront.example.com/app", "*.example.com",
		"storefront..example.com", "-storefront.example.com", "storefront-.example.com",
		strings254(), overlongLabel(),
	} {
		if err := ValidateHostname(hostname); err == nil {
			t.Fatalf("accepted invalid hostname %q", hostname)
		}
	}
}

func TestValidateHostnamesRequiresACompleteSet(t *testing.T) {
	if err := ValidateHostnames(nil); err == nil {
		t.Fatal("accepted a missing hostname set")
	}
	if err := ValidateHostnames([]string{}); err == nil {
		t.Fatal("accepted an empty hostname set")
	}
	if err := ValidateHostnames([]string{"storefront.example.com", "storefront.example.com"}); err == nil {
		t.Fatal("accepted duplicate hostnames")
	}
	if err := ValidateHostnames(hundredOneHostnames()); err == nil {
		t.Fatal("accepted one hundred and one hostnames")
	}
	if err := ValidateHostnames(hundredHostnames()); err != nil {
		t.Fatalf("rejected one hundred hostnames: %v", err)
	}
}

func strings253() string {
	result := "a"
	for len(result) < 63 {
		result += "b"
	}
	result += "."
	for len(result) < 127 {
		result += "c"
	}
	result += "."
	for len(result) < 191 {
		result += "d"
	}
	result += "."
	for len(result) < 253 {
		result += "e"
	}

	return result
}

func strings254() string {
	return strings253() + "f"
}

func overlongLabel() string {
	result := ""
	for len(result) < 64 {
		result += "a"
	}

	return result + ".com"
}

func hundredHostnames() []string {
	result := make([]string, 100)
	for index := range result {
		result[index] = fmt.Sprintf("app-%d.example.com", index)
	}

	return result
}

func hundredOneHostnames() []string {
	return append(hundredHostnames(), "extra.example.com")
}
