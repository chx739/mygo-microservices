package main

import (
	math "math/rand/v2"
	"ride-sharing/shared/util"

	pb "ride-sharing/shared/proto/driver"
	"sync"

	"github.com/mmcloughlin/geohash"
)

type driverInMap struct {
	Driver *pb.Driver
	// Index int
	// TODO: route
}

type Service struct {
	drivers []*driverInMap
	mu      sync.RWMutex
}

func NewService() *Service {
	return &Service{
		drivers: make([]*driverInMap, 0),
	}
}

func (s *Service) RegisterDriver(driverId string, packageSlug string) (*pb.Driver, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	randomIndex := math.IntN(len(PredefinedRoutes))
	randomRoute := PredefinedRoutes[randomIndex]

	// we can ignore this property for now, but it must be sent to the frontend.
	geohash := geohash.Encode(randomRoute[0][0], randomRoute[0][1])

	randomPlate := GenerateRandomPlate()
	randomAvate := util.GetRandomAvatar(randomIndex)

	driver := &pb.Driver{
		Geohash:        geohash,
		Location:       &pb.Location{Latitude: randomRoute[0][0], Longitude: randomRoute[0][1]},
		Name:           "Lando Norris",
		Id:             driverId,
		PackageSlug:    packageSlug,
		ProfilePicture: randomAvate,
		CarPlate:       randomPlate,
	}

	s.drivers = append(s.drivers, &driverInMap{Driver: driver})

	return driver, nil
}

func (s *Service) UnregisterDriver(driverId string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// TODO: Filter driver from list
	for i, d := range s.drivers {
		if d.Driver.Id == driverId {
			s.drivers = append(s.drivers[:i], s.drivers[i+1:]...)
			break
		}
	}
}
