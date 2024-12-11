package main

import (
	"CuTePi/config"
	"fmt"
	"net"
)

// Function to print network interface names and IPv4 addresses
func printNetworkInfo() {
	interfaces, err := net.Interfaces()
	if err != nil {
		fmt.Println("Error fetching network interfaces:", err)
		return
	}

	fmt.Println("Available network interfaces and their IPv4 addresses:")
	for _, iface := range interfaces {
		// Skip down interfaces and those without an IP address
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			// Check if the address is an IPv4 address
			if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil {
				fmt.Printf("Interface: %s, IPv4 Address: %s\n", iface.Name, ipNet.IP.String())
			}
		}
	}

	fmt.Printf("Server is running on port %d\n", config.Port())
	fmt.Println("You can access the app by navigating to this URL in your web browser.")
}
