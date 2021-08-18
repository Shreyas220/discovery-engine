package plugin

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/accuknox/knoxAutoPolicy/src/libs"
	logger "github.com/accuknox/knoxAutoPolicy/src/logging"
	"github.com/accuknox/knoxAutoPolicy/src/types"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	// "github.com/cilium/cilium/pkg/policy/api"
	cilium "github.com/cilium/cilium/api/v1/flow"
	"github.com/cilium/cilium/api/v1/observer"
)

var CiliumReserved string = "reserved:"

var TrafficDirection = map[string]int{
	"TRAFFIC_DIRECTION_UNKNOWN": 0,
	"INGRESS":                   1,
	"EGRESS":                    2,
}

var TraceObservationPoint = map[string]int{
	"UNKNOWN_POINT": 0,
	"TO_PROXY":      1,
	"TO_HOST":       2,
	"TO_STACK":      3,
	"TO_OVERLAY":    4,
	"TO_ENDPOINT":   101,
	"FROM_ENDPOINT": 5,
	"FROM_PROXY":    6,
	"FROM_HOST":     7,
	"FROM_STACK":    8,
	"FROM_OVERLAY":  9,
	"FROM_NETWORK":  10,
	"TO_NETWORK":    11,
}

var Verdict = map[string]int{
	"VERDICT_UNKNOWN": 0,
	"FORWARDED":       1,
	"DROPPED":         2,
	"ERROR":           3,
}

// ======================= //
// == Gloabl Variables  == //
// ======================= //

var CiliumFlows []*cilium.Flow
var CiliumFlowsMutex *sync.Mutex

var log *zerolog.Logger

func init() {
	log = logger.GetInstance()
	CiliumFlowsMutex = &sync.Mutex{}
}

// ====================== //
// == Helper Functions == //
// ====================== //

func convertVerdictToInt(vType interface{}) int {
	return Verdict[vType.(string)]
}

func convertTrafficDirectionToInt(tType interface{}) int {
	return TrafficDirection[tType.(string)]
}

func convertTraceObservationPointToInt(tType interface{}) int {
	return TraceObservationPoint[tType.(string)]
}

func isSynFlagOnly(tcp *cilium.TCP) bool {
	if tcp.Flags != nil && tcp.Flags.SYN && !tcp.Flags.ACK {
		return true
	}
	return false
}

func getL4Ports(l4 *cilium.Layer4) (int, int) {
	if l4.GetTCP() != nil {
		return int(l4.GetTCP().SourcePort), int(l4.GetTCP().DestinationPort)
	} else if l4.GetUDP() != nil {
		return int(l4.GetUDP().SourcePort), int(l4.GetUDP().DestinationPort)
	} else if l4.GetICMPv4() != nil {
		return int(l4.GetICMPv4().Type), int(l4.GetICMPv4().Code)
	} else {
		return -1, -1
	}
}

func getProtocol(l4 *cilium.Layer4) int {
	if l4.GetTCP() != nil {
		return 6
	} else if l4.GetUDP() != nil {
		return 17
	} else if l4.GetICMPv4() != nil {
		return 1
	} else {
		return 0 // unknown?
	}
}

func getProtocolStr(l4 *cilium.Layer4) string {
	if l4.GetTCP() != nil {
		return "tcp"
	} else if l4.GetUDP() != nil {
		return "udp"
	} else if l4.GetICMPv4() != nil {
		return "icmp"
	} else {
		return "unknown" // unknown?
	}
}

func getReservedLabelIfExist(labels []string) string {
	for _, label := range labels {
		if strings.HasPrefix(label, "reserved:") {
			return label
		}
	}

	return ""
}

func getHTTP(flow *cilium.Flow) (string, string) {
	if flow.L7 != nil && flow.L7.GetHttp() != nil {
		if flow.L7.GetType() == 1 { // REQUEST only
			method := flow.L7.GetHttp().GetMethod()
			u, _ := url.Parse(flow.L7.GetHttp().GetUrl())
			path := u.Path

			if strings.HasPrefix(path, "//") {
				path = strings.Replace(path, "//", "/", 1)
			}

			return method, path
		}
	}

	return "", ""
}

// ============================ //
// == Network Flow Convertor == //
// ============================ //

func ConvertCiliumFlowToKnoxNetworkLog(ciliumFlow *cilium.Flow) (types.KnoxNetworkLog, bool) {
	log := types.KnoxNetworkLog{}

	// TODO: packet is dropped (flow.Verdict == 2) and drop reason == 181 (Flows denied by deny policy)?
	if ciliumFlow.Verdict == cilium.Verdict_DROPPED && ciliumFlow.DropReason == 181 {
		return log, false
	}

	// set action
	if ciliumFlow.Verdict == 2 {
		log.Action = "deny"
	} else {
		log.Action = "allow"
	}

	// set EGRESS / INGRESS
	log.Direction = ciliumFlow.GetTrafficDirection().String()

	// set namespace
	if ciliumFlow.Source.Namespace == "" {
		log.SrcNamespace = getReservedLabelIfExist(ciliumFlow.Source.Labels)
	} else {
		log.SrcNamespace = ciliumFlow.Source.Namespace
	}

	if ciliumFlow.Destination.Namespace == "" {
		log.DstNamespace = getReservedLabelIfExist(ciliumFlow.Destination.Labels)
	} else {
		log.DstNamespace = ciliumFlow.Destination.Namespace
	}

	// set pod
	if ciliumFlow.Source.PodName == "" {
		log.SrcPodName = ciliumFlow.IP.Source
	} else {
		log.SrcPodName = ciliumFlow.Source.GetPodName()
	}

	if ciliumFlow.Destination.PodName == "" {
		log.DstPodName = ciliumFlow.IP.Destination
	} else {
		log.DstPodName = ciliumFlow.Destination.GetPodName()
	}

	// get L3
	if ciliumFlow.IP != nil {
		log.SrcIP = ciliumFlow.IP.Source
		log.DstIP = ciliumFlow.IP.Destination
	} else {
		return log, false
	}

	// get L4
	if ciliumFlow.L4 != nil {
		log.Protocol = getProtocol(ciliumFlow.L4)
		if log.Protocol == 6 && ciliumFlow.L4.GetTCP() != nil { // if tcp,
			log.SynFlag = isSynFlagOnly(ciliumFlow.L4.GetTCP())
		}

		log.SrcPort, log.DstPort = getL4Ports(ciliumFlow.L4)
	} else {
		return log, false
	}

	// get L7 HTTP
	if ciliumFlow.GetL7() != nil && ciliumFlow.L7.GetHttp() != nil {
		log.HTTPMethod, log.HTTPPath = getHTTP(ciliumFlow)
		if log.HTTPMethod == "" && log.HTTPPath == "" {
			return log, false
		}
	}

	// get L7 DNS
	if ciliumFlow.GetL7() != nil && ciliumFlow.L7.GetDns() != nil {
		// if DSN response includes IPs
		if ciliumFlow.L7.GetType() == 2 && len(ciliumFlow.L7.GetDns().Ips) > 0 {
			// if internal services, skip
			if strings.HasSuffix(ciliumFlow.L7.GetDns().GetQuery(), "svc.cluster.local.") {
				return log, false
			}

			query := strings.TrimSuffix(ciliumFlow.L7.GetDns().GetQuery(), ".")
			ips := ciliumFlow.L7.GetDns().GetIps()

			log.DNSRes = query
			log.DNSResIPs = []string{}
			for _, ip := range ips {
				log.DNSResIPs = append(log.DNSResIPs, ip)
			}
		}
	}

	return log, true
}

func ConvertMySQLCiliumLogsToKnoxNetworkLogs(docs []map[string]interface{}) []types.KnoxNetworkLog {
	logs := []types.KnoxNetworkLog{}

	for _, doc := range docs {
		ciliumFlow := &cilium.Flow{}
		var err error

		primitiveDoc := map[string]interface{}{
			"traffic_direction": convertTrafficDirectionToInt(doc["traffic_direction"]),
			"verdict":           convertVerdictToInt(doc["verdict"]),
			"policy_match_type": doc["policy_match_type"],
			"drop_reason":       doc["drop_reason"],
		}

		flowByte, err := json.Marshal(primitiveDoc)
		if err != nil {
			log.Error().Msg("Error while unmarshing primitives :" + err.Error())
			continue
		}

		err = json.Unmarshal(flowByte, ciliumFlow)
		if err != nil {
			log.Error().Msg("Error while unmarshing primitives :" + err.Error())
			continue
		}

		if doc["event_type"] != nil {
			err = json.Unmarshal(doc["event_type"].([]byte), &ciliumFlow.EventType)
			if err != nil {
				log.Error().Msg("Error while unmarshing event type :" + err.Error())
				continue
			}
		}

		if doc["source"] != nil {
			err = json.Unmarshal(doc["source"].([]byte), &ciliumFlow.Source)
			if err != nil {
				log.Error().Msg("Error while unmarshing source :" + err.Error())
				continue
			}
		}

		if doc["destination"] != nil {
			err = json.Unmarshal(doc["destination"].([]byte), &ciliumFlow.Destination)
			if err != nil {
				log.Error().Msg("Error while unmarshing destination :" + err.Error())
				continue
			}
		}

		if doc["ip"] != nil {
			err = json.Unmarshal(doc["ip"].([]byte), &ciliumFlow.IP)
			if err != nil {
				log.Error().Msg("Error while unmarshing ip :" + err.Error())
				continue
			}
		}

		if doc["l4"] != nil {
			err = json.Unmarshal(doc["l4"].([]byte), &ciliumFlow.L4)
			if err != nil {
				log.Error().Msg("Error while unmarshing l4 :" + err.Error())
				continue
			}
		}

		if doc["l7"] != nil {
			l7Byte := doc["l7"].([]byte)
			if len(l7Byte) != 0 {
				err = json.Unmarshal(l7Byte, &ciliumFlow.L7)
				if err != nil {
					log.Error().Msg("Error while unmarshing l7 :" + err.Error())
					continue
				}
			}
		}

		if log, valid := ConvertCiliumFlowToKnoxNetworkLog(ciliumFlow); valid {
			// get flow id
			log.FlowID = int(doc["id"].(uint32))

			// get cluster name
			log.ClusterName = doc["cluster_name"].(string)

			logs = append(logs, log)
		}
	}

	return logs
}

func ConvertMongodCiliumLogsToKnoxNetworkLogs(docs []map[string]interface{}) []types.KnoxNetworkLog {
	logs := []types.KnoxNetworkLog{}

	for _, doc := range docs {
		flow := &cilium.Flow{}
		flowByte, _ := json.Marshal(doc)
		if err := json.Unmarshal(flowByte, flow); err != nil {
			log.Error().Msg(err.Error())
			continue
		}

		if log, valid := ConvertCiliumFlowToKnoxNetworkLog(flow); valid {
			logs = append(logs, log)
		}
	}

	return logs
}

func ConvertCiliumNetworkLogsToKnoxNetworkLogs(dbDriver string, docs []map[string]interface{}) []types.KnoxNetworkLog {
	if dbDriver == "mysql" {
		return ConvertMySQLCiliumLogsToKnoxNetworkLogs(docs)
	} else if dbDriver == "mongo" {
		return ConvertMongodCiliumLogsToKnoxNetworkLogs(docs)
	} else {
		return []types.KnoxNetworkLog{}
	}
}

// ============================== //
// == Network Policy Convertor == //
// ============================== //

// TODO: search core-dns? or statically return dns pod
func getCoreDNSEndpoint(services []types.Service) ([]types.CiliumEndpoint, []types.CiliumPortList) {
	matchLabel := map[string]string{
		"k8s:io.kubernetes.pod.namespace": "kube-system",
		"k8s-app":                         "kube-dns",
	}

	coreDNS := []types.CiliumEndpoint{{matchLabel}}

	ciliumPort := types.CiliumPortList{}
	ciliumPort.Ports = []types.CiliumPort{}

	if len(services) == 0 { // add statically
		ciliumPort.Ports = append(ciliumPort.Ports, types.CiliumPort{
			Port: strconv.Itoa(53), Protocol: strings.ToUpper("UDP")},
		)
	} else { // search DNS
		for _, svc := range services {
			if svc.Namespace == "kube-system" && svc.ServiceName == "kube-dns" {
				ciliumPort.Ports = append(ciliumPort.Ports, types.CiliumPort{
					Port: strconv.Itoa(svc.ServicePort), Protocol: strings.ToUpper(svc.Protocol)},
				)
			}
		}
	}

	toPorts := []types.CiliumPortList{ciliumPort}

	// matchPattern
	dnsRules := []types.SubRule{map[string]string{"matchPattern": "*"}}
	toPorts[0].Rules = map[string][]types.SubRule{"dns": dnsRules}

	return coreDNS, toPorts
}

func buildNewCiliumNetworkPolicy(inPolicy types.KnoxNetworkPolicy) types.CiliumNetworkPolicy {
	ciliumPolicy := types.CiliumNetworkPolicy{}

	ciliumPolicy.APIVersion = "cilium.io/v2"
	ciliumPolicy.Kind = "CiliumNetworkPolicy"
	ciliumPolicy.Metadata = map[string]string{}
	for k, v := range inPolicy.Metadata {
		if k == "name" || k == "namespace" {
			ciliumPolicy.Metadata[k] = v
		}
	}

	// update selector matchLabels
	ciliumPolicy.Spec.Selector.MatchLabels = inPolicy.Spec.Selector.MatchLabels

	return ciliumPolicy
}

func ConvertKnoxNetworkPolicyToCiliumPolicy(services []types.Service, inPolicy types.KnoxNetworkPolicy) types.CiliumNetworkPolicy {
	ciliumPolicy := buildNewCiliumNetworkPolicy(inPolicy)

	// ====== //
	// Egress //
	// ====== //
	if len(inPolicy.Spec.Egress) > 0 {
		ciliumPolicy.Spec.Egress = []types.CiliumEgress{}

		for _, knoxEgress := range inPolicy.Spec.Egress {
			ciliumEgress := types.CiliumEgress{}

			// ====================== //
			// build label-based rule //
			// ====================== //
			if knoxEgress.MatchLabels != nil {
				ciliumEgress.ToEndpoints = []types.CiliumEndpoint{{knoxEgress.MatchLabels}}

				// ================ //
				// build L4 toPorts //
				// ================ //
				for _, toPort := range knoxEgress.ToPorts {
					if toPort.Port == "" { // if port number is none, skip
						continue
					}

					if ciliumEgress.ToPorts == nil {
						ciliumEgress.ToPorts = []types.CiliumPortList{}
						ciliumPort := types.CiliumPortList{}
						ciliumPort.Ports = []types.CiliumPort{}
						ciliumEgress.ToPorts = append(ciliumEgress.ToPorts, ciliumPort)

						// =============== //
						// build HTTP rule //
						// =============== //
						if len(knoxEgress.ToHTTPs) > 0 {
							ciliumEgress.ToPorts[0].Rules = map[string][]types.SubRule{}

							httpRules := []types.SubRule{}
							for _, http := range knoxEgress.ToHTTPs {
								// matchPattern
								httpRules = append(httpRules, map[string]string{"method": http.Method,
									"path": http.Path})
							}
							ciliumEgress.ToPorts[0].Rules = map[string][]types.SubRule{"http": httpRules}
						}
					}

					port := types.CiliumPort{Port: toPort.Port, Protocol: strings.ToUpper(toPort.Protocol)}
					ciliumEgress.ToPorts[0].Ports = append(ciliumEgress.ToPorts[0].Ports, port)
				}
			} else if len(knoxEgress.ToCIDRs) > 0 {
				// =============== //
				// build CIDR rule //
				// =============== //
				for _, toCIDR := range knoxEgress.ToCIDRs {
					cidrs := []string{}
					for _, cidr := range toCIDR.CIDRs {
						cidrs = append(cidrs, cidr)
					}
					ciliumEgress.ToCIDRs = cidrs

					// update toPorts if exist
					for _, toPort := range knoxEgress.ToPorts {
						if toPort.Port == "" { // if port number is none, skip
							continue
						}

						if ciliumEgress.ToPorts == nil {
							ciliumEgress.ToPorts = []types.CiliumPortList{}
							ciliumPort := types.CiliumPortList{}
							ciliumPort.Ports = []types.CiliumPort{}
							ciliumEgress.ToPorts = append(ciliumEgress.ToPorts, ciliumPort)
						}

						port := types.CiliumPort{Port: toPort.Port, Protocol: strings.ToUpper(toPort.Protocol)}
						ciliumEgress.ToPorts[0].Ports = append(ciliumEgress.ToPorts[0].Ports, port)
					}
				}
			} else if len(knoxEgress.ToEndtities) > 0 {
				// ================= //
				// build Entity rule //
				// ================= //
				for _, entity := range knoxEgress.ToEndtities {
					if ciliumEgress.ToEntities == nil {
						ciliumEgress.ToEntities = []string{}
					}

					ciliumEgress.ToEntities = append(ciliumEgress.ToEntities, entity)
				}
			} else if len(knoxEgress.ToServices) > 0 {
				// ================== //
				// build Service rule //
				// ================== //
				for _, service := range knoxEgress.ToServices {
					if ciliumEgress.ToServices == nil {
						ciliumEgress.ToServices = []types.CiliumService{}
					}

					ciliumService := types.CiliumService{
						K8sService: types.CiliumK8sService{
							ServiceName: service.ServiceName,
							Namespace:   service.Namespace,
						},
					}

					ciliumEgress.ToServices = append(ciliumEgress.ToServices, ciliumService)
				}
			} else if len(knoxEgress.ToFQDNs) > 0 {
				// =============== //
				// build FQDN rule //
				// =============== //
				for _, fqdn := range knoxEgress.ToFQDNs {
					// TODO: static core-dns
					ciliumEgress.ToEndpoints, ciliumEgress.ToPorts = getCoreDNSEndpoint(services)

					egressFqdn := types.CiliumEgress{}

					if egressFqdn.ToFQDNs == nil {
						egressFqdn.ToFQDNs = []types.CiliumFQDN{}
					}

					// FQDN (+ToPorts)
					for _, matchName := range fqdn.MatchNames {
						egressFqdn.ToFQDNs = append(egressFqdn.ToFQDNs, map[string]string{"matchName": matchName})
					}

					for _, port := range knoxEgress.ToPorts {
						if egressFqdn.ToPorts == nil {
							egressFqdn.ToPorts = []types.CiliumPortList{}
							ciliumPort := types.CiliumPortList{}
							ciliumPort.Ports = []types.CiliumPort{}
							egressFqdn.ToPorts = append(egressFqdn.ToPorts, ciliumPort)
						}

						ciliumPort := types.CiliumPort{Port: port.Port, Protocol: strings.ToUpper(port.Protocol)}
						egressFqdn.ToPorts[0].Ports = append(egressFqdn.ToPorts[0].Ports, ciliumPort)
					}

					ciliumPolicy.Spec.Egress = append(ciliumPolicy.Spec.Egress, egressFqdn)
				}
			}

			ciliumPolicy.Spec.Egress = append(ciliumPolicy.Spec.Egress, ciliumEgress)
		}
	}

	// ======= //
	// Ingress //
	// ======= //
	if len(inPolicy.Spec.Ingress) > 0 {
		ciliumPolicy.Spec.Ingress = []types.CiliumIngress{}

		for _, knoxIngress := range inPolicy.Spec.Ingress {
			ciliumIngress := types.CiliumIngress{}

			// ================= //
			// build label-based //
			// ================= //
			if knoxIngress.MatchLabels != nil {
				ciliumIngress.FromEndpoints = []types.CiliumEndpoint{{knoxIngress.MatchLabels}}

				// ================ //
				// build L4 toPorts //
				// ================ //
				for _, toPort := range knoxIngress.ToPorts {
					if ciliumIngress.ToPorts == nil {
						ciliumIngress.ToPorts = []types.CiliumPortList{}
						ciliumPort := types.CiliumPortList{}
						ciliumPort.Ports = []types.CiliumPort{}
						ciliumIngress.ToPorts = append(ciliumIngress.ToPorts, ciliumPort)

						// =============== //
						// build HTTP rule //
						// =============== //
						if len(knoxIngress.ToHTTPs) > 0 {
							ciliumIngress.ToPorts[0].Rules = map[string][]types.SubRule{}

							httpRules := []types.SubRule{}
							for _, http := range knoxIngress.ToHTTPs {
								// matchPattern
								httpRules = append(httpRules, map[string]string{"method": http.Method,
									"path": http.Path})
							}
							ciliumIngress.ToPorts[0].Rules = map[string][]types.SubRule{"http": httpRules}
						}
					}

					port := types.CiliumPort{Port: toPort.Port, Protocol: strings.ToUpper(toPort.Protocol)}
					ciliumIngress.ToPorts[0].Ports = append(ciliumIngress.ToPorts[0].Ports, port)
				}
			}

			// =============== //
			// build CIDR rule //
			// =============== //
			for _, fromCIDR := range knoxIngress.FromCIDRs {
				for _, cidr := range fromCIDR.CIDRs {
					ciliumIngress.FromCIDRs = append(ciliumIngress.FromCIDRs, cidr)
				}
			}

			// ================= //
			// build Entity rule //
			// ================= //
			for _, entity := range knoxIngress.FromEntities {
				if ciliumIngress.FromEntities == nil {
					ciliumIngress.FromEntities = []string{}
				}
				ciliumIngress.FromEntities = append(ciliumIngress.FromEntities, entity)
			}

			ciliumPolicy.Spec.Ingress = append(ciliumPolicy.Spec.Ingress, ciliumIngress)
		}

	}

	return ciliumPolicy
}

func ConvertKnoxPoliciesToCiliumPolicies(services []types.Service, policies []types.KnoxNetworkPolicy) []types.CiliumNetworkPolicy {
	ciliumPolicies := []types.CiliumNetworkPolicy{}

	for _, policy := range policies {
		ciliumPolicy := ConvertKnoxNetworkPolicyToCiliumPolicy(services, policy)
		ciliumPolicies = append(ciliumPolicies, ciliumPolicy)
	}

	return ciliumPolicies
}

// ========================= //
// == Cilium Hubble Relay == //
// ========================= //

func ConnectHubbleRelay(cfg types.ConfigCiliumHubble) *grpc.ClientConn {
	addr := cfg.HubbleURL + ":" + cfg.HubblePort

	conn, err := grpc.Dial(addr, grpc.WithInsecure())
	if err != nil {
		log.Error().Err(err)
		return nil
	}

	log.Info().Msg("connected to Hubble Relay")
	return conn
}

func GetCiliumFlowsFromHubble(trigger int) []*cilium.Flow {
	results := []*cilium.Flow{}

	CiliumFlowsMutex.Lock()
	if len(CiliumFlows) == 0 {
		log.Info().Msgf("Cilium hubble traffic flow not exist")
		CiliumFlowsMutex.Unlock()
		return results
	}

	if len(CiliumFlows) < trigger {
		log.Info().Msgf("The number of cilium hubble traffic flow [%d] is less than trigger [%d]", len(CiliumFlows), trigger)
		CiliumFlowsMutex.Unlock()
		return results
	}

	results = CiliumFlows          // copy
	CiliumFlows = []*cilium.Flow{} // reset
	CiliumFlowsMutex.Unlock()

	fisrtDoc := results[0]
	lastDoc := results[len(results)-1]

	// id/time filter update
	startTime := fisrtDoc.Time.Seconds
	endTime := lastDoc.Time.Seconds

	log.Info().Msgf("The total number of cilium hubble traffic flow: [%d] from %s ~ to %s", len(results),
		time.Unix(startTime, 0).Format(libs.TimeFormSimple),
		time.Unix(endTime, 0).Format(libs.TimeFormSimple))

	return results
}

func StartHubbleRelay(StopChan chan struct{}, wg *sync.WaitGroup, cfg types.ConfigCiliumHubble) {
	conn := ConnectHubbleRelay(cfg)
	defer conn.Close()
	defer wg.Done()

	client := observer.NewObserverClient(conn)

	req := &observer.GetFlowsRequest{
		Follow:    true,
		Whitelist: nil,
		Blacklist: nil,
		Since:     timestamppb.Now(),
		Until:     nil,
	}

	if stream, err := client.GetFlows(context.Background(), req); err == nil {
		for {
			select {
			case <-StopChan:
				return

			default:
				res, err := stream.Recv()
				if err != nil {
					log.Error().Msg("Cilium network flow stream stopped: " + err.Error())
					return
				}

				switch r := res.ResponseTypes.(type) {
				case *observer.GetFlowsResponse_Flow:
					flow := r.Flow

					CiliumFlowsMutex.Lock()
					CiliumFlows = append(CiliumFlows, flow)
					CiliumFlowsMutex.Unlock()
				}
			}
		}
	} else {
		log.Error().Msg("Unable to stream network flow: " + err.Error())
	}
}
