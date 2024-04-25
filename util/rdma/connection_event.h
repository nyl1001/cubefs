#ifndef CONNECTION_EVENT_H
#define CONNECTION_EVENT_H

//#include "wait_group.h"
//#include "queue.h"
#include "hashmap.h"
#include "rdma_proto.h"

#include "log.h"

#include "connection.h"

#include <rdma/rdma_cma.h>
#include <rdma/rdma_verbs.h>


void on_addr_resolved(struct rdma_cm_id *id);

void on_route_resolved(struct rdma_cm_id *id);

void on_accept(struct rdma_cm_id* listen_id, struct rdma_cm_id* id);

void on_connected(struct rdma_cm_id *id);

void on_disconnected(struct rdma_cm_id* id);

void process_cm_event(struct rdma_cm_id *conn_id, struct rdma_cm_id *listen_id, int event_type);

extern void *cm_thread(void *ctx);


#endif